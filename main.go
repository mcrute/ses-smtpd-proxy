package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"code.crute.us/mcrute/ses-smtpd-proxy/smtpd"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ses"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/api/auth/approle"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var version string

const (
	SesSizeLimit = 10000000
	DefaultAddr  = ":2500"
)

var (
	emailSent = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "smtpd",
		Name:      "email_send_success_total",
		Help:      "Total number of successfuly sent emails",
	})
	emailError = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "smtpd",
		Name:      "email_send_fail_total",
		Help:      "Total number emails that failed to send",
	}, []string{"type"})
	sesError = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "smtpd",
		Name:      "ses_error_total",
		Help:      "Total number errors with SES",
	})
	credentialRenewalSuccess = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "smtpd",
		Name:      "credential_renewal_success_total",
		Help:      "Total number successful credential renewals",
	})
	credentialRenewalError = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "smtpd",
		Name:      "credential_renewal_error_total",
		Help:      "Total number errors during credential renewal",
	})
)

type Envelope struct {
	from          string
	client        *ses.SES
	configSetName *string
	rcpts         []*string
	b             bytes.Buffer
}

func (e *Envelope) AddRecipient(rcpt smtpd.MailAddress) error {
	email := rcpt.Email()
	e.rcpts = append(e.rcpts, &email)
	return nil
}

func (e *Envelope) BeginData() error {
	if len(e.rcpts) == 0 {
		emailError.With(prometheus.Labels{"type": "no valid recipients"}).Inc()
		return smtpd.SMTPError("554 5.5.1 Error: no valid recipients")
	}
	return nil
}

func (e *Envelope) Write(line []byte) error {
	e.b.Write(line)
	if e.b.Len() > SesSizeLimit { // SES limitation
		emailError.With(prometheus.Labels{"type": "minimum message size exceed"}).Inc()
		log.Printf("message size %d exceeds SES limit of %d", e.b.Len(), SesSizeLimit)
		return smtpd.SMTPError("554 5.5.1 Error: maximum message size exceeded")
	}
	return nil
}

func (e *Envelope) logMessageSend() {
	dr := make([]string, len(e.rcpts))
	for i := range e.rcpts {
		dr[i] = *e.rcpts[i]
	}
	log.Printf("sending message from %+v to %+v", e.from, dr)
	emailSent.Inc()
}

func (e *Envelope) Close() error {
	r := &ses.SendRawEmailInput{
		ConfigurationSetName: e.configSetName,
		Source:               &e.from,
		Destinations:         e.rcpts,
		RawMessage:           &ses.RawMessage{Data: e.b.Bytes()},
	}
	_, err := e.client.SendRawEmail(r)
	if err != nil {
		log.Printf("ERROR: ses: %v", err)
		emailError.With(prometheus.Labels{"type": "ses error"}).Inc()
		sesError.Inc()
		return smtpd.SMTPError("451 4.5.1 Temporary server error. Please try again later")
	}
	e.logMessageSend()
	return err
}

func logRenewal(renewal *api.RenewOutput) {
	canRenew := "renewable"
	if !renewal.Secret.Renewable {
		canRenew = "not renewable"
	}
	leaseID := renewal.Secret.LeaseID
	if leaseID == "" && renewal.Secret.MountType == "token" {
		leaseID = "vault_token"
	}
	log.Printf("Successfully renewed lease '%s' at %s for %s, %s",
		leaseID,
		renewal.RenewedAt.Format(time.RFC3339),
		time.Duration(renewal.Secret.LeaseDuration)*time.Second,
		canRenew,
	)
}

func renewSecret(vc *api.Client, s *api.Secret, credentialError chan<- error) error {
	w, err := vc.NewLifetimeWatcher(&api.LifetimeWatcherInput{Secret: s})
	if err != nil {
		return err
	}
	go w.Start()

	go func() {
		for {
			select {
			case err := <-w.DoneCh():
				if err != nil {
					credentialRenewalError.Inc()
					credentialError <- err
				}
			case renewal := <-w.RenewCh():
				credentialRenewalSuccess.Inc()
				logRenewal(renewal)
			}
		}
	}()

	return nil
}

func getVaultSecret(ctx context.Context, path string, credentialError chan<- error) (credentials.Value, error) {
	var r credentials.Value

	vc, err := api.NewClient(api.DefaultConfig())
	if err != nil {
		return r, err
	}

	// Use AppRole if it's in the environment, otherwise assume VAULT_TOKEN
	// was provided in the environment.
	if roleID := os.Getenv("VAULT_APPROLE_ROLE_ID"); roleID != "" {
		appRoleAuth, err := approle.NewAppRoleAuth(roleID, &approle.SecretID{
			FromEnv: "VAULT_APPROLE_SECRET_ID",
		})
		if err != nil {
			return r, fmt.Errorf("unable to initialize AppRole auth method: %w", err)
		}
		if loginSecret, err := vc.Auth().Login(ctx, appRoleAuth); err != nil {
			return r, fmt.Errorf("unable to login to AppRole auth method: %w", err)
		} else {
			if err := renewSecret(vc, loginSecret, credentialError); err != nil {
				return r, err
			}
		}
	}

	secret, err := vc.Logical().Read(path)
	if err != nil {
		return r, err
	}
	if secret == nil {
		return r, fmt.Errorf("Vault returned no AWS secret")
	}

	keyId, ok := secret.Data["access_key"]
	if !ok {
		return r, fmt.Errorf("Vault secret had no access_key")
	}

	secretKey, ok := secret.Data["secret_key"]
	if !ok {
		return r, fmt.Errorf("Vault secret had no secret_key")
	}

	r.AccessKeyID = keyId.(string)
	r.SecretAccessKey = secretKey.(string)

	return r, renewSecret(vc, secret, credentialError)
}

func makeSesClient(ctx context.Context, enableVault bool, vaultPath string, credentialError chan<- error) (*ses.SES, error) {
	var err error
	var s *session.Session

	if enableVault {
		cred, err := getVaultSecret(ctx, vaultPath, credentialError)
		if err != nil {
			return nil, err
		}

		s, err = session.NewSession(&aws.Config{
			Credentials: credentials.NewStaticCredentialsFromCreds(cred),
		})
	} else {
		s, err = session.NewSession()
	}
	if err != nil {
		return nil, err
	}

	return ses.New(s), nil
}

func main() {
	var err error

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	disablePrometheus := flag.Bool("disable-prometheus", false, "Disables prometheus metrics server")
	prometheusBind := flag.String("prometheus-bind", ":2501", "Address/port on which to bind Prometheus server")
	enableVault := flag.Bool("enable-vault", false, "Enable fetching AWS IAM credentials from a Vault server")
	vaultPath := flag.String("vault-path", "", "Full path to Vault credential (ex: \"aws/creds/my-mail-user\")")
	showVersion := flag.Bool("version", false, "Show program version")
	configurationSetName := flag.String("configuration-set-name", "", "Configuration set name with which SendRawEmail will be invoked")
	enableHealthCheck := flag.Bool("enable-health-check", false, "Enable health check server")
	healthCheckBind := flag.String("health-check-bind", ":3000", "Address/port on which to bind health check server")

	flag.Parse()

	if *showVersion {
		fmt.Printf("ses-smtp-proxy version %s\n", version)
		return
	}

	if *enableHealthCheck {
		sm := http.NewServeMux()
		ps := &http.Server{Addr: *healthCheckBind, Handler: sm}
		sm.Handle("/health", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("Content-Type", "application/json")
			w.Write([]byte("{\"name\": \"ses-smtp-proxy\", \"status\": \"ok\", \"version\": \"" + version + "\"}"))
		}))
		go ps.ListenAndServe()
	}

	credentialError := make(chan error, 2)
	sesClient, err := makeSesClient(ctx, *enableVault, *vaultPath, credentialError)
	if err != nil {
		log.Fatalf("Error creating AWS session: %s", err)
	}

	addr := DefaultAddr
	if flag.Arg(0) != "" {
		addr = flag.Arg(0)
	} else if flag.NArg() > 1 {
		log.Fatalf("usage: %s [listen_host:port]", os.Args[0])
	}

	if !*disablePrometheus {
		sm := http.NewServeMux()
		ps := &http.Server{Addr: *prometheusBind, Handler: sm}
		sm.Handle("/metrics", promhttp.Handler())
		go ps.ListenAndServe()
	}

	if *configurationSetName == "" {
		configurationSetName = nil
	}

	s := &smtpd.Server{
		Addr: addr,
		OnNewMail: func(c smtpd.Connection, from smtpd.MailAddress) (smtpd.Envelope, error) {
			return &Envelope{
				from:          from.Email(),
				client:        sesClient,
				configSetName: configurationSetName,
			}, nil
		},
	}

	go func() {
		log.Printf("ListenAndServe on %s", addr)
		if err := s.ListenAndServe(); err != nil {
			log.Printf("Error in ListenAndServe: %v", err)
		}
	}()

	select {
	case <-ctx.Done():
		log.Printf("SIGTERM/SIGINT received, shutting down")
		os.Exit(0)
	case err := <-credentialError:
		log.Fatalf("Error renewing credential: %s", err)
		os.Exit(1)
	}
}
