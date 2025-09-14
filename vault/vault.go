package vault

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/api/auth/approle"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
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

type vaultJwtAuth struct {
	JWT  string
	Role string
}

func (a *vaultJwtAuth) Login(ctx context.Context, client *api.Client) (*api.Secret, error) {
	loginData := map[string]any{
		"jwt": a.JWT,
	}
	if a.Role != "" {
		loginData["role"] = a.Role
	}

	res, err := client.Logical().WriteWithContext(ctx, "auth/jwt/login", loginData)
	if err != nil {
		return nil, fmt.Errorf("unable to log in with jwt auth: %w", err)
	}

	return res, nil
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
	// Don't attempt to renew a secret that can't be renewed otherwise
	// LifetimeWatcher will fail to build.
	if !s.Renewable {
		return nil
	}

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

func GetVaultSecret(ctx context.Context, path string, credentialError chan<- error) (credentials.Value, error) {
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

	if vaultJwt := os.Getenv("VAULT_JWT"); vaultJwt != "" {
		jwtAuth := &vaultJwtAuth{
			JWT:  vaultJwt,
			Role: os.Getenv("VAULT_JWT_ROLE"),
		}
		if loginSecret, err := vc.Auth().Login(ctx, jwtAuth); err != nil {
			return r, fmt.Errorf("unable to login to VaultJWT auth method: %w", err)
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

	// If this is a KV secret then it will be nested within an additional
	// level of JSON with a "data" top level key. If it's an AWS type
	// credential it will not have that nesting. Otherwise the inner keys
	// are expected to be the same. Noramalize that here.
	var data map[string]any = secret.Data
	if d, ok := secret.Data["data"]; ok {
		if dd, ok := d.(map[string]any); ok {
			data = dd
		}
	}

	keyId, ok := data["access_key"]
	if !ok {
		return r, fmt.Errorf("Vault secret had no access_key")
	}

	secretKey, ok := data["secret_key"]
	if !ok {
		return r, fmt.Errorf("Vault secret had no secret_key")
	}

	r.AccessKeyID = keyId.(string)
	r.SecretAccessKey = secretKey.(string)

	return r, renewSecret(vc, secret, credentialError)
}
