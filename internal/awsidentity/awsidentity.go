// Package awsidentity configures AWS credentials for local and Google Cloud workloads.
package awsidentity

import (
	"context"
	"strings"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials/idtoken"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/justinswe/std/errors"
)

const (
	credentialExpiryWindow = 5 * time.Minute
	identityTokenTimeout   = 5 * time.Second
	roleSessionName        = "jarvis-worker"
)

const credentialExpiryJitter = 0.2

// Config selects the AWS credential source.
type Config struct {
	RoleARN  string
	Audience string
}

// Load returns an AWS configuration using Google federation or the default credential chain.
func Load(ctx context.Context, cfg Config) (aws.Config, error) {
	roleARN := strings.TrimSpace(cfg.RoleARN)
	audience := strings.TrimSpace(cfg.Audience)
	if roleARN == "" && audience == "" {
		loaded, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return aws.Config{}, errors.Wrap(err, "load default AWS configuration")
		}
		return loaded, nil
	}
	if roleARN == "" || audience == "" {
		return aws.Config{}, errors.New("AWS role ARN and web identity audience must be configured together")
	}

	tokens, err := idtoken.NewCredentials(&idtoken.Options{
		Audience:           audience,
		ComputeTokenFormat: idtoken.ComputeTokenFormatStandard,
	})
	if err != nil {
		return aws.Config{}, errors.Wrap(err, "initialize Google identity token provider")
	}
	loaded, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithCredentialsProvider(aws.AnonymousCredentials{}))
	if err != nil {
		return aws.Config{}, errors.Wrap(err, "load AWS configuration for Google federation")
	}
	loaded.Credentials = newCredentialsProvider(tokens, sts.NewFromConfig(loaded), roleARN, identityTokenTimeout)
	return loaded, nil
}

type identityTokenRetriever struct {
	provider auth.TokenProvider
	timeout  time.Duration
}

func (r identityTokenRetriever) GetIdentityToken() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	token, err := r.provider.Token(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "retrieve Google identity token")
	}
	if token == nil || strings.TrimSpace(token.Value) == "" {
		return nil, errors.New("Google identity token is empty")
	}
	return []byte(token.Value), nil
}

func newCredentialsProvider(
	tokens auth.TokenProvider,
	client stscreds.AssumeRoleWithWebIdentityAPIClient,
	roleARN string,
	tokenTimeout time.Duration,
) aws.CredentialsProvider {
	provider := stscreds.NewWebIdentityRoleProvider(
		client,
		roleARN,
		identityTokenRetriever{provider: tokens, timeout: tokenTimeout},
		func(options *stscreds.WebIdentityRoleOptions) {
			options.RoleSessionName = roleSessionName
		},
	)
	return aws.NewCredentialsCache(provider, func(options *aws.CredentialsCacheOptions) {
		options.ExpiryWindow = credentialExpiryWindow
		options.ExpiryWindowJitterFrac = credentialExpiryJitter
	})
}
