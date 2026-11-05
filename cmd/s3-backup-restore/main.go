package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"s3-backup-restore/internal"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("Operation failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("s3-backup-restore", flag.ContinueOnError)
	verbose := flags.Bool("v", false, "Show debug output (without HTTP headers or bodies).")
	veryVerbose := flags.Bool("vv", false, "Alias for -v; HTTP payload logging is disabled.")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: s3-backup-restore [options] backup (hourly|daily|weekly|monthly) | restore | cron")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() == 0 {
		flags.Usage()
		return errors.New("a command is required")
	}
	command := flags.Arg(0)
	if command != "backup" && command != "restore" && command != "cron" {
		return fmt.Errorf("unknown command %q", command)
	}
	if command == "backup" && flags.NArg() != 2 {
		return errors.New("backup requires a type: hourly, daily, weekly, or monthly")
	}
	if command != "backup" && flags.NArg() != 1 {
		return errors.New("unexpected command arguments")
	}
	level := slog.LevelInfo
	if *verbose || *veryVerbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		return errors.New("S3_BUCKET is required")
	}
	directory := envString("DATA_DIRECTORY", "/data")
	if directory == "/" {
		return errors.New("DATA_DIRECTORY must not be the filesystem root")
	}
	prefix := strings.TrimRight(envString("S3_PATH", "/"), "/")
	counts := [4]int{}
	for i, kind := range []string{"HOURLY", "DAILY", "WEEKLY", "MONTHLY"} {
		// Accept the old direct-backup names as a fallback for existing installations.
		raw := envString("NUM_BACKUPS_"+kind, envString("NUM_"+kind+"_BACKUPS", "3"))
		count, err := strconv.Atoi(raw)
		if err != nil || count < 0 {
			return fmt.Errorf("NUM_BACKUPS_%s must be a nonnegative integer", kind)
		}
		counts[i] = count
	}
	if err := loadSecretEnvironment(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithClientLogMode(0))
	if err != nil {
		return fmt.Errorf("load AWS configuration: %w", err)
	}
	client := s3.NewFromConfig(cfg)
	uploads := transfermanager.New(client)
	backup := internal.Backup{
		HourlyBackups: counts[0], DailyBackups: counts[1], WeeklyBackups: counts[2], MonthlyBackups: counts[3],
		S3Bucket: bucket, S3Path: prefix, DataDirectory: directory, S3Service: client, UploadClient: uploads,
	}
	switch command {
	case "backup":
		return backup.Run(ctx, flags.Arg(1))
	case "restore":
		permissions, err := strconv.ParseUint(envString("DIRECTORY_PERMISSIONS", "0755"), 8, 32)
		if err != nil || permissions > 0777 {
			return errors.New("DIRECTORY_PERMISSIONS must be an octal mode between 0000 and 0777")
		}
		uid, err := envID("CHOWN_UID", 1000)
		if err != nil {
			return err
		}
		gid, err := envID("CHOWN_GID", 1000)
		if err != nil {
			return err
		}
		chown, err := envBool("CHOWN_ENABLE")
		if err != nil {
			return err
		}
		force, err := envBool("RESTORE_FORCE")
		if err != nil {
			return err
		}
		restore := internal.Restore{
			S3Bucket: bucket, S3Path: prefix, DataDirectory: directory, S3Service: client,
			NewDirectoryPermissions: os.FileMode(permissions), ChownEnable: chown, ChownUID: uid, ChownGID: gid,
			ForceRestore: force, RestoreFile: os.Getenv("RESTORE_FILE"),
		}
		return restore.Run(ctx)
	case "cron":
		scheduler := internal.Cron{
			HourlyCadence: envString("CADENCE_HOURLY", "0 * * * *"), DailyCadence: envString("CADENCE_DAILY", "10 1 * * *"),
			WeeklyCadence: envString("CADENCE_WEEKLY", "10 2 * * 0"), MonthlyCadence: envString("CADENCE_MONTHLY", "10 3 1 * *"),
			HourlyBackups: counts[0], DailyBackups: counts[1], WeeklyBackups: counts[2], MonthlyBackups: counts[3],
			S3Bucket: bucket, S3Path: prefix, DataDirectory: directory, S3Service: client, UploadClient: uploads,
		}
		return scheduler.Run(ctx)
	}
	return nil
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envID(name string, fallback int) (int, error) {
	value, err := strconv.Atoi(envString(name, strconv.Itoa(fallback)))
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a nonnegative integer", name)
	}
	return value, nil
}

func envBool(name string) (bool, error) {
	value, err := strconv.ParseBool(envString(name, "false"))
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return value, nil
}

func loadSecretEnvironment() error {
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		if os.Getenv(name) != "" {
			continue
		}
		value, err := os.ReadFile("/run/secrets/" + name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read secret %s: %w", name, err)
		}
		if err := os.Setenv(name, strings.TrimSpace(string(value))); err != nil {
			return err
		}
	}
	return nil
}
