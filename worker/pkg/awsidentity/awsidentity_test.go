package awsidentity

import (
	"context"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/auth"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeTokenProvider struct {
	mu    sync.Mutex
	token *auth.Token
	err   error
	wait  bool
	calls int
}

func (p *fakeTokenProvider) Token(ctx context.Context) (*auth.Token, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.token, p.err
}

func (p *fakeTokenProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type fakeSTSClient struct {
	mu        sync.Mutex
	startOnce sync.Once
	inputs    []*sts.AssumeRoleWithWebIdentityInput
	outputs   []*sts.AssumeRoleWithWebIdentityOutput
	err       error
	started   chan struct{}
	release   chan struct{}
}

func (c *fakeSTSClient) AssumeRoleWithWebIdentity(
	_ context.Context,
	input *sts.AssumeRoleWithWebIdentityInput,
	_ ...func(*sts.Options),
) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	c.mu.Lock()
	c.inputs = append(c.inputs, input)
	if c.err != nil {
		c.mu.Unlock()
		return nil, c.err
	}
	output := c.outputs[0]
	if len(c.outputs) > 1 {
		c.outputs = c.outputs[1:]
	}
	started, release := c.started, c.release
	c.mu.Unlock()
	if started != nil {
		c.startOnce.Do(func() { close(started) })
	}
	if release != nil {
		<-release
	}
	return output, nil
}

func (c *fakeSTSClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.inputs)
}

func stsOutput(accessKey string, expiration time.Time) *sts.AssumeRoleWithWebIdentityOutput {
	return &sts.AssumeRoleWithWebIdentityOutput{
		AssumedRoleUser: &ststypes.AssumedRoleUser{
			Arn:           aws.String("arn:aws:sts::123060663424:assumed-role/JarvisCloudRunDynamoDB/jarvis-worker"),
			AssumedRoleId: aws.String("role-id:jarvis-worker"),
		},
		Credentials: &ststypes.Credentials{
			AccessKeyId:     aws.String(accessKey),
			SecretAccessKey: aws.String("secret"),
			SessionToken:    aws.String("session"),
			Expiration:      aws.Time(expiration),
		},
		SubjectFromWebIdentityToken: aws.String("117567217391646394249"),
	}
}

func TestCredentialsProviderExchangesAndCachesToken(t *testing.T) {
	tokens := &fakeTokenProvider{token: &auth.Token{Value: "google-token"}}
	client := &fakeSTSClient{outputs: []*sts.AssumeRoleWithWebIdentityOutput{stsOutput("access-key", time.Now().Add(time.Hour))}}
	provider := newCredentialsProvider(tokens, client, "arn:aws:iam::123060663424:role/JarvisCloudRunDynamoDB", time.Second)

	first, err := provider.Retrieve(t.Context())
	require.NoError(t, err)
	second, err := provider.Retrieve(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "access-key", first.AccessKeyID)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, tokens.callCount())
	assert.Equal(t, 1, client.callCount())
	assert.Equal(t, "jarvis-worker", aws.ToString(client.inputs[0].RoleSessionName))
	assert.Equal(t, "google-token", aws.ToString(client.inputs[0].WebIdentityToken))
	assert.Equal(t, "arn:aws:iam::123060663424:role/JarvisCloudRunDynamoDB", aws.ToString(client.inputs[0].RoleArn))
}

func TestCredentialsProviderRefreshesExpiringCredentials(t *testing.T) {
	tokens := &fakeTokenProvider{token: &auth.Token{Value: "google-token"}}
	client := &fakeSTSClient{outputs: []*sts.AssumeRoleWithWebIdentityOutput{
		stsOutput("expiring", time.Now().Add(time.Minute)),
		stsOutput("refreshed", time.Now().Add(time.Hour)),
	}}
	provider := newCredentialsProvider(tokens, client, "role", time.Second)

	first, err := provider.Retrieve(t.Context())
	require.NoError(t, err)
	second, err := provider.Retrieve(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "expiring", first.AccessKeyID)
	assert.Equal(t, "refreshed", second.AccessKeyID)
	assert.Equal(t, 2, client.callCount())
}

func TestCredentialsProviderCoalescesConcurrentRetrievals(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	client := &fakeSTSClient{
		outputs: []*sts.AssumeRoleWithWebIdentityOutput{stsOutput("access-key", time.Now().Add(time.Hour))},
		started: started,
		release: release,
	}
	provider := newCredentialsProvider(
		&fakeTokenProvider{token: &auth.Token{Value: "google-token"}}, client, "role", time.Second,
	)

	const callers = 10
	errors := make(chan error, callers)
	for range callers {
		go func() {
			_, err := provider.Retrieve(t.Context())
			errors <- err
		}()
	}
	<-started
	close(release)
	for range callers {
		require.NoError(t, <-errors)
	}
	assert.Equal(t, 1, client.callCount())
}

func TestCredentialsProviderRejectsEmptyToken(t *testing.T) {
	client := &fakeSTSClient{outputs: []*sts.AssumeRoleWithWebIdentityOutput{stsOutput("unused", time.Now().Add(time.Hour))}}
	provider := newCredentialsProvider(&fakeTokenProvider{token: &auth.Token{}}, client, "role", time.Second)

	_, err := provider.Retrieve(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Google identity token is empty")
	assert.Equal(t, 0, client.callCount())
}

func TestCredentialsProviderBoundsTokenRetrieval(t *testing.T) {
	provider := newCredentialsProvider(&fakeTokenProvider{wait: true}, &fakeSTSClient{}, "role", 10*time.Millisecond)

	_, err := provider.Retrieve(t.Context())

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestCredentialsProviderDoesNotExposeTokenInSTSError(t *testing.T) {
	client := &fakeSTSClient{err: errors.New("STS rejected identity")}
	provider := newCredentialsProvider(&fakeTokenProvider{token: &auth.Token{Value: "sensitive-google-token"}}, client, "role", time.Second)

	_, err := provider.Retrieve(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "STS rejected identity")
	assert.NotContains(t, err.Error(), "sensitive-google-token")
}

func TestConfigRequiresRoleAndAudienceTogether(t *testing.T) {
	for _, cfg := range []Config{
		{RoleARN: "role"},
		{Audience: "audience"},
	} {
		_, err := Load(t.Context(), cfg)
		require.EqualError(t, err, "AWS role ARN and web identity audience must be configured together")
	}
}
