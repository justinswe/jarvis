// Package usage meters and limits per-guild Jarvis usage in Valkey.
package usage

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/justinswe/jarvis/worker/pkg/discord"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/jarvis/worker/pkg/valkeyconn"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"github.com/valkey-io/valkey-go"
	"go.uber.org/zap"
)

const (
	// defaultKeyPrefix namespaces every key written by this package.
	defaultKeyPrefix = "jarvis"
	// closeFlushTimeout bounds the final flush performed during shutdown.
	closeFlushTimeout = 5 * time.Second
)

// Limits is one subscription tier's enforcement budget.
type Limits struct {
	RequestsPerSecond int
	Burst             int
	TokensPerHour     int
}

// Config configures metering and limit enforcement. It says nothing about how Valkey is
// reached, so it is all NewWithClient needs; New pairs it with a Connection.
type Config struct {
	KeyPrefix     string
	Timeout       time.Duration
	FlushInterval time.Duration
	RequestTTL    time.Duration
	TokenTTL      time.Duration
	Tiers         map[string]Limits
	DefaultTier   string
	WarnThreshold int
}

// Connection is where and how to reach Valkey. Only New uses it: a caller supplying an
// already-dialed client through NewWithClient has settled all of this already, and asking
// for it again would mean requiring fields this package would then ignore.
type Connection struct {
	Addresses []string
	Username  string
	Password  string
	SelectDB  int
	TLS       bool
	// DialTimeout bounds the connection and its verifying PING. Deliberately separate
	// from Config.Timeout, which bounds the inline admission check: a deadline tight
	// enough for one round trip cannot also cover a TLS handshake and authentication.
	DialTimeout time.Duration
}

// Client meters and limits per-guild Jarvis usage in Valkey.
type Client struct {
	commands  commander
	meter     *meter
	cfg       Config
	stop      chan struct{}
	stopped   sync.WaitGroup
	closeOnce sync.Once
	warnedMu  sync.Mutex
	warned    map[string]struct{}
}

// New dials Valkey, verifies connectivity, and starts the metering flush loop.
//
// The dialed client is owned by the returned Client and closed with it. To share one
// Valkey connection across multiple Valkey-backed dependencies, dial with valkeyconn.Dial
// and use NewWithClient instead.
func New(ctx context.Context, connection Connection, cfg Config) (*Client, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	commands, err := newValkeyCommander(ctx, connection)
	if err != nil {
		return nil, err
	}
	return newClient(commands, cfg), nil
}

// NewWithClient starts the metering flush loop over an already-connected, externally
// owned Valkey client. Close does not close client; the caller remains responsible for it.
func NewWithClient(client valkey.Client, cfg Config) (*Client, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	return newClient(&valkeyCommander{client: client}, cfg), nil
}

// newClient starts a usage client over an already-connected commander.
func newClient(commands commander, cfg Config) *Client {
	client := &Client{
		commands: commands, meter: newMeter(), cfg: cfg,
		stop: make(chan struct{}), warned: make(map[string]struct{}),
	}
	client.stopped.Add(1)
	go client.flushLoop()
	return client
}

// normalized validates the configuration and applies defaults.
func (c Config) normalized() (Config, error) {
	if strings.TrimSpace(c.KeyPrefix) == "" {
		c.KeyPrefix = defaultKeyPrefix
	}
	if c.Timeout <= 0 {
		return Config{}, errors.New("Valkey timeout must be positive")
	}
	if c.FlushInterval <= 0 {
		return Config{}, errors.New("Valkey flush interval must be positive")
	}
	if c.RequestTTL <= 0 || c.TokenTTL <= 0 {
		return Config{}, errors.New("Valkey retention durations must be positive")
	}
	if c.WarnThreshold < 1 || c.WarnThreshold > 100 {
		return Config{}, errors.New("Valkey warning threshold must be between 1 and 100")
	}
	if len(c.Tiers) > 0 {
		if _, known := c.Tiers[c.DefaultTier]; !known {
			return Config{}, errors.Errorf("default tier %q is not defined", c.DefaultTier)
		}
	}
	return c, nil
}

// Close stops the flush loop after one final synchronous flush.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.stop)
		c.stopped.Wait()
		// The worker's deferred Close runs after its signal context is cancelled, so the
		// final flush needs a context that outlives it. Background is already
		// uncancellable, so it needs no WithoutCancel.
		ctx, cancel := context.WithTimeout(context.Background(), closeFlushTimeout)
		defer cancel()
		c.flush(ctx)
		c.commands.close()
	})
	return nil
}

// Allow admits one guild request, records it, and fails open on any Valkey failure.
func (c *Client) Allow(ctx context.Context, guildID, tier string) (discord.Admission, error) {
	if guildID == "" {
		return discord.Admission{Allowed: true}, nil
	}
	effective, limits := c.limitsFor(tier)
	base := guildBase(c.cfg.KeyPrefix, guildID)
	args := []string{
		base, effective,
		strconv.Itoa(limits.RequestsPerSecond), strconv.Itoa(limits.Burst), strconv.Itoa(limits.TokensPerHour),
		strconv.Itoa(int(c.cfg.RequestTTL / time.Second)),
	}
	callCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	reply, err := c.commands.allow(callCtx, gcraKey(base), args)
	if err != nil {
		return discord.Admission{Allowed: true}, err
	}
	if len(reply) < allowReplyLength {
		return discord.Admission{Allowed: true}, errors.New("malformed admission reply")
	}
	admission := discord.Admission{
		Allowed:    reply[allowIndexAdmitted] == 1,
		NearLimit:  reply[allowIndexUtilized] >= int64(c.cfg.WarnThreshold),
		RetryAfter: time.Duration(reply[allowIndexRetryMS]) * time.Millisecond,
	}
	if !admission.Allowed {
		app.L().Info("Guild request denied by usage limits",
			zap.String("guild_id", guildID), zap.String("tier", effective),
			zap.String("deny_kind", denyKindName(reply[allowIndexDenyKind])),
			zap.Duration("retry_after", admission.RetryAfter),
		)
	}
	return admission, nil
}

// RecordUsage accumulates one model round's token usage in memory.
func (c *Client) RecordUsage(report genai.UsageReport) {
	if report.GuildID == "" {
		return
	}
	effective, _ := c.limitsFor(report.Tier)
	c.meter.add(
		meterKey{guildID: report.GuildID, tier: effective, model: modelField(report)},
		meterDelta{
			input:     int64(report.Usage.InputTokens),
			output:    int64(report.Usage.OutputTokens),
			reasoning: int64(report.Usage.ReasoningTokens),
			total:     int64(report.Usage.TotalTokens),
			calls:     int64(report.Calls),
		},
	)
}

// limitsFor resolves the effective tier name and its limits, falling back when unknown.
func (c *Client) limitsFor(tier string) (string, Limits) {
	if len(c.cfg.Tiers) == 0 {
		return tier, Limits{}
	}
	if limits, known := c.cfg.Tiers[tier]; known {
		return tier, limits
	}
	if tier != "" {
		c.warnUnknownTier(tier)
	}
	return c.cfg.DefaultTier, c.cfg.Tiers[c.cfg.DefaultTier]
}

// warnUnknownTier logs each unrecognized tier name once per process.
func (c *Client) warnUnknownTier(tier string) {
	c.warnedMu.Lock()
	defer c.warnedMu.Unlock()
	if _, seen := c.warned[tier]; seen {
		return
	}
	c.warned[tier] = struct{}{}
	app.L().Warn("Guild tier is not defined by this deployment; using the default tier",
		zap.String("tier", tier), zap.String("default_tier", c.cfg.DefaultTier))
}

// flushLoop writes accumulated token deltas on a fixed interval until Close.
func (c *Client) flushLoop() {
	defer c.stopped.Done()
	ticker := time.NewTicker(c.cfg.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), c.cfg.FlushInterval)
			c.flush(ctx)
			cancel()
		}
	}
}

// flush writes one drained accumulation window, dropping deltas that cannot be written.
func (c *Client) flush(ctx context.Context) {
	deltas, dropped := c.meter.drain()
	if dropped > 0 {
		app.L().Warn("Dropped guild usage deltas above the aggregator cap", zap.Int("dropped", dropped))
	}
	if len(deltas) == 0 {
		return
	}
	pending := batches(c.cfg.KeyPrefix, deltas)
	if err := c.commands.flush(ctx, pending, c.cfg.TokenTTL); err != nil {
		app.L().Warn("Failed to flush guild usage to Valkey", zap.Int("guilds", len(pending)), zap.Error(err))
		return
	}
	guildIDs := make([]string, 0, len(pending))
	for _, batch := range pending {
		guildIDs = append(guildIDs, batch.guildID)
	}
	day := time.Now().UTC().Unix() / int64(24*time.Hour/time.Second)
	if err := c.commands.indexGuilds(ctx, guildIndexKey(c.cfg.KeyPrefix, day), guildIDs, c.cfg.TokenTTL); err != nil {
		app.L().Debug("Failed to index active guilds", zap.Error(err))
	}
}

// modelField renders one report's provider-qualified model identifier.
func modelField(report genai.UsageReport) string {
	if report.Provider == "" {
		return report.ModelID
	}
	return report.Provider + "/" + report.ModelID
}

// denyKindName renders the admission script's denial reason for logs.
func denyKindName(kind int64) string {
	switch kind {
	case denyKindRate:
		return "requests_per_second"
	case denyKindTokens:
		return "token_budget"
	default:
		return "none"
	}
}

// commander runs the usage scripts against Valkey.
type commander interface {
	allow(ctx context.Context, slotKey string, args []string) ([]int64, error)
	flush(ctx context.Context, pending []guildBatch, ttl time.Duration) error
	indexGuilds(ctx context.Context, indexKey string, guildIDs []string, ttl time.Duration) error
	close()
}

// valkeyCommander is the production commander backed by a live Valkey client.
type valkeyCommander struct {
	client valkey.Client
	// owns marks a client dialed by this package, which close must therefore close.
	// A client supplied through NewWithClient is owned by its caller instead.
	owns bool
}

// newValkeyCommander dials Valkey and verifies the connection before returning.
func newValkeyCommander(ctx context.Context, connection Connection) (*valkeyCommander, error) {
	addresses := make([]string, 0, len(connection.Addresses))
	for _, address := range connection.Addresses {
		if trimmed := strings.TrimSpace(address); trimmed != "" {
			addresses = append(addresses, trimmed)
		}
	}
	if len(addresses) == 0 {
		return nil, errors.New("at least one Valkey address is required")
	}
	if connection.DialTimeout <= 0 {
		return nil, errors.New("Valkey dial timeout must be positive")
	}
	client, err := valkeyconn.Dial(ctx, valkeyconn.Config{
		Addresses: addresses,
		Username:  connection.Username,
		Password:  connection.Password,
		SelectDB:  connection.SelectDB,
		TLS:       connection.TLS,
		Timeout:   connection.DialTimeout,
	})
	if err != nil {
		return nil, err
	}
	return &valkeyCommander{client: client, owns: true}, nil
}

// allow runs the admission script for one guild.
func (c *valkeyCommander) allow(ctx context.Context, slotKey string, args []string) ([]int64, error) {
	result, err := allowScript.Exec(ctx, c.client, []string{slotKey}, args).ToArray()
	if err != nil {
		return nil, errors.Wrap(err, "run Valkey admission script")
	}
	reply := make([]int64, 0, len(result))
	for _, value := range result {
		decoded, err := value.AsInt64()
		if err != nil {
			return nil, errors.Wrap(err, "decode Valkey admission reply")
		}
		reply = append(reply, decoded)
	}
	return reply, nil
}

// flush applies every guild's token deltas in one batched script invocation.
func (c *valkeyCommander) flush(ctx context.Context, pending []guildBatch, ttl time.Duration) error {
	if len(pending) == 0 {
		return nil
	}
	ttlSeconds := strconv.Itoa(int(ttl / time.Second))
	executions := make([]valkey.LuaExec, 0, len(pending))
	for _, batch := range pending {
		args := append([]string{batch.base, batch.tier, ttlSeconds}, batch.args...)
		executions = append(executions, valkey.LuaExec{Keys: []string{gcraKey(batch.base)}, Args: args})
	}
	for _, result := range flushScript.ExecMulti(ctx, c.client, executions...) {
		if err := result.Error(); err != nil {
			return errors.Wrap(err, "run Valkey flush script")
		}
	}
	return nil
}

// indexGuilds records the guilds active in one UTC day so readers avoid scanning keys.
func (c *valkeyCommander) indexGuilds(ctx context.Context, indexKey string, guildIDs []string, ttl time.Duration) error {
	if len(guildIDs) == 0 {
		return nil
	}
	commands := []valkey.Completed{
		c.client.B().Sadd().Key(indexKey).Member(guildIDs...).Build(),
		c.client.B().Expire().Key(indexKey).Seconds(int64(ttl / time.Second)).Build(),
	}
	for _, result := range c.client.DoMulti(ctx, commands...) {
		if err := result.Error(); err != nil {
			return errors.Wrap(err, "index active guilds")
		}
	}
	return nil
}

// close releases the underlying Valkey connections.
func (c *valkeyCommander) close() {
	if c.owns {
		c.client.Close()
	}
}
