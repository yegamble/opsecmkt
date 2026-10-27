package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"time"

	"opsecmkt/internal/market"
	"opsecmkt/internal/moderation"
)

func runModerationWorker(ctx context.Context, args []string, out io.Writer) error {
	f := flag.NewFlagSet("moderation-worker", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	enabled := f.Bool("enabled", os.Getenv("MODERATION_ENABLED") == "on", "enable transactional enqueueing and run the worker")
	disable := f.Bool("disable", false, "disable new enqueueing and exit; preserve existing jobs")
	once := f.Bool("once", false, "process one bounded batch and exit")
	replay := f.Bool("replay", false, "revisit durable snapshots under current pinned policy")
	interval := f.Duration("interval", time.Minute, "poll interval (10 seconds to 1 hour)")
	batch := f.Int("batch", 20, "maximum jobs per pass (1 to 100)")
	dbFile := f.String("database-url-file", os.Getenv("DATABASE_URL_FILE"), "private worker database URL file")
	modelEnabled := f.Bool("model", false, "enable optional local inference; absent/failing model queues human review")
	socket := f.String("model-socket", os.Getenv("MODERATION_MODEL_SOCKET"), "absolute Unix socket path; TCP is unsupported")
	weights := f.String("weights", os.Getenv("MODERATION_WEIGHTS_PATH"), "reviewed local weights file")
	weightsHash := f.String("weights-sha256", os.Getenv("MODERATION_WEIGHTS_SHA256"), "pinned lowercase SHA256")
	modelID := f.String("model-id", os.Getenv("MODERATION_MODEL_ID"), "bounded configured model identity")
	prompt := f.String("prompt-version", moderation.PromptVersion, "pinned prompt revision")
	modelTimeout := f.Duration("model-timeout", 20*time.Second, "maximum inference request duration")
	sla := f.Duration("sla", 24*time.Hour, "human review SLA; expiry always HOLD")
	confidence := f.Float64("confidence-floor", .9, "minimum model confidence")
	newActorDays := f.Int("new-actor-days", 14, "actor age threshold for punitive proposals")
	abuseThreshold := f.Int("abuse-threshold", 5, "per actor one hour review threshold")
	limits := market.DefaultModerationLimits()
	maxPending := f.Int64("max-pending", limits.MaxPending, "maximum active moderation jobs")
	maxRecords := f.Int64("max-records", limits.MaxRecords, "maximum lifetime moderation allocation slots")
	maxBytes := f.Int64("max-reserved-bytes", limits.MaxBytes, "maximum reserved moderation payload bytes; excludes database/WAL overhead")
	recipientPath := f.String("escalation-recipient", os.Getenv("MODERATION_ESCALATION_RECIPIENT"), "offline OpenPGP public recipient")
	signingPath := f.String("signing-key", os.Getenv("MODERATION_SIGNING_KEY_FILE"), "private separate Ed25519 seed file for encrypted escalation bundles")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(out, "usage: opsecmkt moderation-worker [--enabled | --disable] [--once] [--replay] [--database-url-file FILE] [--interval 1m] [--batch 20]\nOptional local inference: --model --model-socket PATH --weights FILE --weights-sha256 HEX --model-id ID --prompt-version moderation-v5\nReview policy: --confidence-floor 0.9 --new-actor-days 14 --abuse-threshold 5 --sla 24h\nCapacity: --max-pending 256 --max-records 4096 --max-reserved-bytes 268435456; three active jobs per actor; a lifetime budget flag below the stored value is ignored\nEncrypted private queue export: --escalation-recipient PUBLIC_KEY_FILE --signing-key PRIVATE_SEED_FILE\nReplay skips completed metadata-only jobs. Disabled by default. No target, account or payment mutation. Inference never runs in the web process.")
			return nil
		}
		return errors.New("moderation-worker: invalid flags")
	}
	if *disable {
		explicitEnabled := false
		f.Visit(func(value *flag.Flag) {
			if value.Name == "enabled" {
				explicitEnabled = true
			}
		})
		if !explicitEnabled {
			*enabled = false
		}
	}
	if len(f.Args()) != 0 || *enabled && *disable || *interval < 10*time.Second || *interval > time.Hour || *batch < 1 || *batch > 100 || *sla < time.Minute || *sla > 365*24*time.Hour || *modelTimeout < time.Second || *modelTimeout > 30*time.Second || *prompt != moderation.PromptVersion || *confidence <= 0 || *confidence > 1 || math.IsNaN(*confidence) || *newActorDays < 1 || *newActorDays > 3650 || *abuseThreshold < 1 || *abuseThreshold > 1000 {
		return errors.New("moderation-worker: invalid configuration")
	}
	if value := os.Getenv("MODERATION_ENABLED"); value != "" && value != "on" && value != "off" {
		return errors.New("moderation-worker: MODERATION_ENABLED must be on or off")
	}
	if *maxPending < 1 || *maxPending > 10000 || *maxRecords < 1 || *maxRecords > 1000000 || *maxBytes < 512<<10 || *maxBytes > 1<<40 {
		return errors.New("moderation-worker: invalid capacity limits")
	}
	limits = market.ModerationLimits{MaxPending: *maxPending, MaxRecords: *maxRecords, MaxBytes: *maxBytes}
	capacityFlags := make(map[string]bool)
	f.Visit(func(value *flag.Flag) {
		if value.Name == "max-pending" || value.Name == "max-records" || value.Name == "max-reserved-bytes" {
			capacityFlags[value.Name] = true
		}
	})
	if !*enabled && !*disable {
		fmt.Fprintln(out, "moderation-worker: disabled")
		return nil
	}
	if *modelEnabled {
		if !regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`).MatchString(*modelID) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(*weightsHash) {
			return errors.New("moderation-worker: explicit model identity and weights pin required")
		}
	} else {
		// Disabled inference must not inherit unused endpoints or model metadata.
		*modelID, *weightsHash = "", ""
	}
	dsn := os.Getenv("DATABASE_URL")
	if *dbFile != "" {
		if dsn != "" {
			return errors.New("moderation-worker: database inputs conflict")
		}
		b, err := readModerationPrivate(*dbFile)
		if err != nil {
			return errors.New("moderation-worker: private database input unavailable")
		}
		dsn = string(b)
	}
	if dsn == "" {
		return errors.New("moderation-worker: database input required")
	}
	db, err := market.OpenDB(dsn)
	if err != nil {
		return errors.New("moderation-worker: database configuration invalid")
	}
	// The worker holds the application's database authority and runs from a clean environment, so the production
	// profile's DSN check (OPSECMKT_PROFILE) does not reach it: refuse an unverified remote connection here, before
	// the first dial (A-682).
	if !market.DatabaseTransportVerified(dsn) {
		db.Close()
		return errors.New("moderation-worker: remote database requires sslmode=verify-full")
	}
	defer db.Close()
	db.SetMaxOpenConns(3)
	db.SetMaxIdleConns(2)
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err = db.PingContext(checkCtx); err != nil {
		return errors.New("moderation-worker: database unavailable")
	}
	if *disable {
		if err = market.SetModerationEnabled(checkCtx, db, false); err != nil {
			return errors.New("moderation-worker: queue configuration unavailable")
		}
		fmt.Fprintln(out, "moderation-worker: enqueueing disabled; durable jobs preserved")
		return nil
	}
	// Migration 146 provides the backfill sweep every pass runs, and 152 the status columns every pass writes (without
	// them each pass would fail and be retried rather than report the missing upgrade).
	var migrated bool
	if err = db.QueryRowContext(checkCtx, `SELECT count(*)=2 FROM schema_migrations WHERE version IN (146,152)`).Scan(&migrated); err != nil || !migrated {
		return errors.New("moderation-worker: application migrations required before enabling queue")
	}
	cfg := moderation.DefaultConfig()
	cfg.Enabled = *modelEnabled
	cfg.PromptVersion = *prompt
	cfg.ModelID = *modelID
	cfg.WeightsHash = *weightsHash
	cfg.HumanSLA = *sla
	cfg.ConfidenceFloor = *confidence
	cfg.NewActorDays = *newActorDays
	cfg.AbuseThreshold = *abuseThreshold
	w := market.ModerationWorker{DB: db, Engine: moderation.Engine{Config: cfg}, Lease: 2 * time.Minute, Interval: *interval}
	if (*recipientPath == "") != (*signingPath == "") {
		return errors.New("moderation-worker: escalation recipient and signing key must both be set")
	}
	if *recipientPath != "" {
		w.Recipient, err = market.LoadAuditExportRecipient(*recipientPath)
		if err != nil || w.Recipient == nil {
			return errors.New("moderation-worker: escalation recipient unavailable")
		}
		seed, err := readModerationPrivate(*signingPath)
		if err != nil {
			return errors.New("moderation-worker: escalation signer unavailable")
		}
		decoded, err := hex.DecodeString(string(seed))
		if err != nil || len(decoded) != ed25519.SeedSize {
			return errors.New("moderation-worker: escalation signer invalid")
		}
		w.SigningKey = ed25519.NewKeyFromSeed(decoded)
	}
	if *modelEnabled {
		local, modelErr := moderation.NewLocalModel(moderation.LocalModelConfig{SocketPath: *socket, WeightsPath: *weights, WeightsHash: *weightsHash, ModelID: *modelID, PromptVersion: *prompt, Timeout: *modelTimeout})
		if modelErr != nil {
			w.ModelFailure = "model_unavailable"
		} else {
			w.Engine.Model = local
		}
	}
	if len(capacityFlags) > 0 {
		if err = applyModerationCapacityFlags(ctx, db, limits, capacityFlags, out); err != nil {
			return err
		}
	}
	if err = market.SetModerationEnabled(ctx, db, true); err != nil {
		return errors.New("moderation-worker: queue configuration unavailable; run application migrations first")
	}
	if *replay {
		queued, skipped, replayErr := market.ReplayModerationJobs(ctx, db)
		if replayErr != nil {
			return errors.New("moderation-worker: replay unavailable")
		}
		fmt.Fprintf(out, "moderation-worker: replay queued=%d skipped_metadata_only=%d\n", queued, skipped)
	}
	// A failed pass (a database restart or outage) is retried with a bounded backoff rather than ending the worker,
	// which would leave capture enqueueing with nothing processing it (A-683). Every job stays durable; the first
	// pass after the outage records it as db_unavailable. Configuration errors above still exit.
	var backoff time.Duration
	for {
		processed, passErr := w.Pass(ctx, *batch)
		wait := *interval
		if passErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			if *once {
				return errors.New("moderation-worker: pass unavailable; durable jobs preserved")
			}
			backoff = moderationWorkerBackoff(backoff, *interval)
			wait = backoff
			fmt.Fprintf(out, "moderation-worker: pass unavailable; durable jobs preserved; retrying in %s\n", backoff)
		} else {
			backoff = 0
			fmt.Fprintf(out, "moderation-worker: processed=%d\n", processed)
			if *once {
				return nil
			}
		}
		if moderationWorkerSleep(ctx, wait) != nil {
			return nil
		}
	}
}

// moderationWorkerSleep waits d or until ctx ends (tests replace it).
var moderationWorkerSleep = func(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// moderationWorkerBackoff is the wait after a failed pass: 5 seconds, doubling with each consecutive failure, never
// more than the pass interval or 5 minutes, so a recovered database is used again within one interval.
func moderationWorkerBackoff(previous, interval time.Duration) time.Duration {
	return min(max(2*previous, 5*time.Second), interval, 5*time.Minute)
}

// applyModerationCapacityFlags stores the explicitly named limits. Restarting a controller preserves the stored
// policy: a partial update changes only the named limits, and the lifetime budget (--max-records,
// --max-reserved-bytes) only grows here. An administrator can raise it on the private moderation panel without a
// restart, so a flag below the stored value is an older setting: it is ignored with a fixed line rather than lowering
// the budget or exiting below the allocations made since (A-654). Lowering is a host-only procedure
// (docs/moderation-agent.md). The capacity row is locked first, so a concurrent panel raise is never undone.
func applyModerationCapacityFlags(ctx context.Context, db *sql.DB, limits market.ModerationLimits, named map[string]bool, out io.Writer) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("moderation-worker: capacity configuration unavailable")
	}
	defer tx.Rollback()
	var current market.ModerationLimits
	if err = tx.QueryRowContext(ctx, `SELECT max_pending,max_records,max_bytes FROM moderation_capacity WHERE singleton FOR UPDATE`).Scan(&current.MaxPending, &current.MaxRecords, &current.MaxBytes); err != nil {
		return errors.New("moderation-worker: capacity configuration unavailable")
	}
	stored := current
	if named["max-pending"] {
		current.MaxPending = limits.MaxPending
	}
	for _, budget := range []struct {
		flag   string
		value  int64
		stored *int64
	}{{"max-records", limits.MaxRecords, &current.MaxRecords}, {"max-reserved-bytes", limits.MaxBytes, &current.MaxBytes}} {
		if !named[budget.flag] {
			continue
		}
		if budget.value < *budget.stored {
			fmt.Fprintln(out, "moderation-worker: stored moderation budget is higher; flag ignored (--"+budget.flag+")")
			continue
		}
		*budget.stored = budget.value
	}
	if err = market.ConfigureModerationLimits(ctx, tx, current); err != nil {
		return errors.New("moderation-worker: capacity configuration unavailable or below existing allocations")
	}
	// A-687: a change made by host command is audited without an actor, as -set-role is (admin_role.go); flags that
	// leave the stored limits as they were write nothing.
	if current != stored {
		if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(action) VALUES($1)`, "Moderation limits changed from "+moderationLimitsText(stored)+" to "+moderationLimitsText(current)+" by host command"); err != nil {
			return errors.New("moderation-worker: capacity configuration unavailable")
		}
	}
	if err = tx.Commit(); err != nil {
		return errors.New("moderation-worker: capacity configuration unavailable")
	}
	return nil
}

func moderationLimitsText(v market.ModerationLimits) string {
	return fmt.Sprintf("max-pending %d, max-records %d, max-reserved-bytes %d", v.MaxPending, v.MaxRecords, v.MaxBytes)
}

// Read only the role's explicitly selected secret. Never import application,
// wallet or Tor secrets into a worker merely because their _FILE inputs exist.
func readModerationPrivate(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private regular file required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) || !actual.Mode().IsRegular() || actual.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private file changed")
	}
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) > 65536 {
		return nil, errors.New("private file exceeds limit")
	}
	b = bytes.TrimSuffix(b, []byte{'\n'})
	if len(b) == 0 || bytes.ContainsAny(b, "\x00\r\n") {
		return nil, errors.New("one private line required")
	}
	return b, nil
}
