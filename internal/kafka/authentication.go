// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kafka // import "github.com/open-telemetry/opentelemetry-collector-contrib/internal/kafka"

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"fmt"

	"github.com/IBM/sarama"
	"github.com/aws/aws-msk-iam-sasl-signer-go/signer"
	"go.opentelemetry.io/collector/config/configtls"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/kafka/awsmsk"
)

// Authentication defines authentication.
type Authentication struct {
	PlainText *PlainTextConfig        `mapstructure:"plain_text"`
	SASL      *SASLConfig             `mapstructure:"sasl"`
	TLS       *configtls.ClientConfig `mapstructure:"tls"`
	Kerberos  *KerberosConfig         `mapstructure:"kerberos"`
}

// PlainTextConfig defines plaintext authentication.
type PlainTextConfig struct {
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
}

// SASLConfig defines the configuration for the SASL authentication.
type SASLConfig struct {
	// Username to be used on authentication
	Username string `mapstructure:"username"`
	// Password to be used on authentication
	Password string `mapstructure:"password"`
	// SASL Mechanism to be used, possible values are: (PLAIN, AWS_MSK_IAM, AWS_MSK_IAM_OAUTHBEARER, SCRAM-SHA-256 or SCRAM-SHA-512).
	Mechanism string `mapstructure:"mechanism"`
	// SASL Protocol Version to be used, possible values are: (0, 1). Defaults to 0.
	Version int `mapstructure:"version"`

	AWSMSK AWSMSKConfig `mapstructure:"aws_msk"`
}

// AWSMSKConfig defines the additional SASL authentication
// measures needed to use AWS_MSK_IAM and AWS_MSK_IAM_OAUTHBEARER mechanism
type AWSMSKConfig struct {
	// Region is the AWS region the MSK cluster is based in
	Region string `mapstructure:"region"`
	// BrokerAddr is the client is connecting to in order to perform the auth required
	BrokerAddr string `mapstructure:"broker_addr"`
	// Context
	ctx context.Context
}

// Token return the AWS session token for the AWS_MSK_IAM_OAUTHBEARER mechanism
func (c *AWSMSKConfig) Token() (*sarama.AccessToken, error) {
	token, _, err := signer.GenerateAuthToken(c.ctx, c.Region)

	return &sarama.AccessToken{Token: token}, err
}

// KerberosConfig defines kerberos configuration.
type KerberosConfig struct {
	ServiceName     string `mapstructure:"service_name"`
	Realm           string `mapstructure:"realm"`
	UseKeyTab       bool   `mapstructure:"use_keytab"`
	Username        string `mapstructure:"username"`
	Password        string `mapstructure:"password" json:"-"`
	ConfigPath      string `mapstructure:"config_file"`
	KeyTabPath      string `mapstructure:"keytab_file"`
	DisablePAFXFAST bool   `mapstructure:"disable_fast_negotiation"`
}

// ConfigureAuthentication configures authentication in sarama.Config.
func ConfigureAuthentication(ctx context.Context, config Authentication, saramaConfig *sarama.Config) error {
	if config.PlainText != nil {
		configurePlaintext(*config.PlainText, saramaConfig)
	}
	if config.TLS != nil {
		if err := configureTLS(*config.TLS, saramaConfig); err != nil {
			return err
		}
	}
	if config.SASL != nil {
		if err := configureSASL(ctx, *config.SASL, saramaConfig); err != nil {
			return err
		}
	}

	if config.Kerberos != nil {
		configureKerberos(*config.Kerberos, saramaConfig)
	}
	return nil
}

func configurePlaintext(config PlainTextConfig, saramaConfig *sarama.Config) {
	saramaConfig.Net.SASL.Enable = true
	saramaConfig.Net.SASL.User = config.Username
	saramaConfig.Net.SASL.Password = config.Password
}

func configureSASL(ctx context.Context, config SASLConfig, saramaConfig *sarama.Config) error {
	if config.Username == "" && config.Mechanism != "AWS_MSK_IAM_OAUTHBEARER" {
		return fmt.Errorf("username have to be provided")
	}

	if config.Password == "" && config.Mechanism != "AWS_MSK_IAM_OAUTHBEARER" {
		return fmt.Errorf("password have to be provided")
	}

	saramaConfig.Net.SASL.Enable = true
	saramaConfig.Net.SASL.User = config.Username
	saramaConfig.Net.SASL.Password = config.Password

	switch config.Mechanism {
	case "SCRAM-SHA-512":
		saramaConfig.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient { return &XDGSCRAMClient{HashGeneratorFcn: sha512.New} }
		saramaConfig.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA512
	case "SCRAM-SHA-256":
		saramaConfig.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient { return &XDGSCRAMClient{HashGeneratorFcn: sha256.New} }
		saramaConfig.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA256
	case "PLAIN":
		saramaConfig.Net.SASL.Mechanism = sarama.SASLTypePlaintext
	case "AWS_MSK_IAM":
		saramaConfig.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient {
			return awsmsk.NewIAMSASLClient(config.AWSMSK.BrokerAddr, config.AWSMSK.Region, saramaConfig.ClientID)
		}
		saramaConfig.Net.SASL.Mechanism = awsmsk.Mechanism
	case "AWS_MSK_IAM_OAUTHBEARER":
		config.AWSMSK.ctx = ctx
		saramaConfig.Net.SASL.Mechanism = sarama.SASLTypeOAuth
		saramaConfig.Net.SASL.TokenProvider = &config.AWSMSK
		tlsConfig := tls.Config{}
		saramaConfig.Net.TLS.Enable = true
		saramaConfig.Net.TLS.Config = &tlsConfig
	default:
		return fmt.Errorf(`invalid SASL Mechanism %q: can be either "PLAIN", "AWS_MSK_IAM", "AWS_MSK_IAM_OAUTHBEARER", "SCRAM-SHA-256" or "SCRAM-SHA-512"`, config.Mechanism)
	}

	switch config.Version {
	case 0:
		saramaConfig.Net.SASL.Version = sarama.SASLHandshakeV0
	case 1:
		saramaConfig.Net.SASL.Version = sarama.SASLHandshakeV1
	default:
		return fmt.Errorf(`invalid SASL Protocol Version %d: can be either 0 or 1`, config.Version)
	}

	return nil
}

func configureTLS(config configtls.ClientConfig, saramaConfig *sarama.Config) error {
	tlsConfig, err := config.LoadTLSConfig(context.Background())
	if err != nil {
		return fmt.Errorf("error loading tls config: %w", err)
	}
	saramaConfig.Net.TLS.Enable = true
	saramaConfig.Net.TLS.Config = tlsConfig
	return nil
}

func configureKerberos(config KerberosConfig, saramaConfig *sarama.Config) {
	saramaConfig.Net.SASL.Mechanism = sarama.SASLTypeGSSAPI
	saramaConfig.Net.SASL.Enable = true
	if config.UseKeyTab {
		saramaConfig.Net.SASL.GSSAPI.KeyTabPath = config.KeyTabPath
		saramaConfig.Net.SASL.GSSAPI.AuthType = sarama.KRB5_KEYTAB_AUTH
	} else {
		saramaConfig.Net.SASL.GSSAPI.AuthType = sarama.KRB5_USER_AUTH
		saramaConfig.Net.SASL.GSSAPI.Password = config.Password
	}
	saramaConfig.Net.SASL.GSSAPI.KerberosConfigPath = config.ConfigPath
	saramaConfig.Net.SASL.GSSAPI.Username = config.Username
	saramaConfig.Net.SASL.GSSAPI.Realm = config.Realm
	saramaConfig.Net.SASL.GSSAPI.ServiceName = config.ServiceName
	saramaConfig.Net.SASL.GSSAPI.DisablePAFXFAST = config.DisablePAFXFAST
}
