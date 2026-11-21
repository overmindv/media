package config

import "testing"

func TestLocalScanBypass(t *testing.T) {
	t.Setenv("MEDIA_INTERNAL_TOKEN", "test-token")
	for _, tc := range []struct {
		name, env, flag string
		want, wantError bool
	}{
		{name: "strict by default in local", env: "local", flag: "false"},
		{name: "explicit local bypass", env: "local", flag: "true", want: true},
		{name: "production cannot bypass", env: "production", flag: "true", wantError: true},
		{name: "environment is required", flag: "true", wantError: true},
		{name: "typo fails startup", env: "local", flag: "tru", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ENV", tc.env)
			t.Setenv("MEDIA_ALLOW_UNSCANNED_LOCAL", tc.flag)
			got, err := Load()
			if (err != nil) != tc.wantError || got.AllowUnscannedLocal != tc.want {
				t.Errorf("bypass = %t, err = %v", got.AllowUnscannedLocal, err)
			}
		})
	}
}
