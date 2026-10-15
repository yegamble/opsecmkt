package market

import "testing"

func TestRetentionConfig(t *testing.T) {
	for _, value := range []string{"", "off", "false", "on", "true", "yes", "ON"} {
		for _, dir := range []string{"", "relative", "/tmp/retention/../state", "/tmp/retention-state"} {
			t.Run(value+":"+dir, func(t *testing.T) {
				cfg, err := retentionConfigFromEnv(func(name string) string {
					if name == "RETENTION_ENABLED" {
						return value
					}
					if name == "RETENTION_STATE_DIR" {
						return dir
					}
					return ""
				})
				enabled := value == "on" || value == "true"
				validValue := enabled || value == "" || value == "off" || value == "false"
				valid := validValue && (!enabled || dir == "/tmp/retention-state")
				if (err == nil) != valid {
					t.Fatalf("enabled=%q directory=%q error=%v", value, dir, err)
				}
				if valid && cfg.Enabled != enabled {
					t.Fatalf("enabled=%v want%v", cfg.Enabled, enabled)
				}
			})
		}
	}
}
