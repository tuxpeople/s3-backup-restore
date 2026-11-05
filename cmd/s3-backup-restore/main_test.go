package main

import (
	"testing"
)

func TestInvalidConfigurationReturnsError(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"unknown command", []string{"unknown"}, nil},
		{"missing backup type", []string{"backup"}, nil},
		{"negative retention", []string{"backup", "hourly"}, map[string]string{"NUM_BACKUPS_HOURLY": "-1"}},
		{"invalid octal mode", []string{"restore"}, map[string]string{"DIRECTORY_PERMISSIONS": "0899"}},
		{"negative uid", []string{"restore"}, map[string]string{"CHOWN_UID": "-1"}},
		{"invalid boolean", []string{"restore"}, map[string]string{"RESTORE_FORCE": "typo"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("S3_BUCKET", "test-bucket")
			t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
			t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
			t.Setenv("AWS_REGION", "us-west-2")
			for name, value := range test.env {
				t.Setenv(name, value)
			}
			if err := run(test.args); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestHelpWorksWithoutAWSCredentials(t *testing.T) {
	if err := run([]string{"-h"}); err != nil {
		t.Fatal(err)
	}
}
