package internal

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/robfig/cron/v3"
)

type Cron struct {
	HourlyCadence, DailyCadence, WeeklyCadence, MonthlyCadence string
	HourlyBackups, DailyBackups, WeeklyBackups, MonthlyBackups int
	S3Bucket, S3Path, DataDirectory                            string
	S3Service                                                  S3API
	UploadClient                                               UploadAPI
}

func (c Cron) Run(ctx context.Context) error {
	scheduler := cron.New()
	backup := Backup{
		HourlyBackups: c.HourlyBackups, DailyBackups: c.DailyBackups,
		WeeklyBackups: c.WeeklyBackups, MonthlyBackups: c.MonthlyBackups,
		S3Bucket: c.S3Bucket, S3Path: c.S3Path, DataDirectory: c.DataDirectory,
		S3Service: c.S3Service, UploadClient: c.UploadClient,
	}
	for _, job := range []struct{ kind, cadence string }{
		{"hourly", c.HourlyCadence}, {"daily", c.DailyCadence}, {"weekly", c.WeeklyCadence}, {"monthly", c.MonthlyCadence},
	} {
		count, err := backup.retention(job.kind)
		if err != nil {
			return err
		}
		if count == 0 {
			continue
		}
		if _, err := scheduler.AddFunc(job.cadence, func() {
			if err := backup.Run(ctx, job.kind); err != nil {
				slog.Error("Backup failed", "type", job.kind, "error", err)
			}
		}); err != nil {
			return fmt.Errorf("invalid %s cadence: %w", job.kind, err)
		}
	}
	scheduler.Start()
	slog.Info("Starting cron")
	<-ctx.Done()
	// Wait for canceled jobs to finish cleaning up their own temporary files.
	stopped := scheduler.Stop()
	<-stopped.Done()
	return nil
}
