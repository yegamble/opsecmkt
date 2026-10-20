package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
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
			fmt.Fprintln(out, "usage: opsecmkt moderation-worker [--enabled | --disable] [--once] [--replay] [--database-url-file FILE] [--interval 1m] [--batch 20]\nOptional local inference: --model --model-socket PATH --weights FILE --weights-sha256 HEX --model-id ID --prompt-version moderation-v2\nReview policy: --confidence-floor 0.9 --new-actor-days 14 --abuse-threshold 5 --sla 24h\nCapacity: --max-pending 256 --max-records 4096 --max-reserved-bytes 268435456; three active jobs per actor\nEncrypted private queue export: --escalation-recipient PUBLIC_KEY_FILE --signing-key PRIVATE_SEED_FILE\nReplay skips completed metadata-only jobs. Disabled by default. No target, account or payment mutation. Inference never runs in the web process.")
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
	var migrated bool
	if err = db.QueryRowContext(checkCtx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=129)`).Scan(&migrated); err != nil || !migrated {
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
	w := market.ModerationWorker{DB: db, Engine: moderation.Engine{Config: cfg}, Lease: 2 * time.Minute}
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
		// Restarting a controller preserves the stored policy. A partial explicit
		// update changes only the named limits, never resets other approved values.
		var current market.ModerationLimits
		if err = db.QueryRowContext(ctx, `SELECT max_pending,max_records,max_bytes FROM moderation_capacity WHERE singleton`).Scan(&current.MaxPending, &current.MaxRecords, &current.MaxBytes); err != nil {
			return errors.New("moderation-worker: capacity configuration unavailable")
		}
		if capacityFlags["max-pending"] {
			current.MaxPending = limits.MaxPending
		}
		if capacityFlags["max-records"] {
			current.MaxRecords = limits.MaxRecords
		}
		if capacityFlags["max-reserved-bytes"] {
			current.MaxBytes = limits.MaxBytes
		}
		if err = market.ConfigureModerationLimits(ctx, db, current); err != nil {
			return errors.New("moderation-worker: capacity configuration unavailable or below existing allocations")
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
	for {
		processed, passErr := w.Pass(ctx, *batch)
		if passErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("moderation-worker: pass unavailable; durable jobs preserved")
		}
		fmt.Fprintf(out, "moderation-worker: processed=%d\n", processed)
		if *once {
			return nil
		}
		timer := time.NewTimer(*interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
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
