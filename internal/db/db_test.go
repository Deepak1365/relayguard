package db

import (
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestForceSessionTimeZone(t *testing.T) {
	params := map[string]string{
		"TimeZone":         "Europe/Berlin",
		"timezone":         "America/New_York",
		"TIMEZONE":         "Asia/Tokyo",
		"application_name": "relayguard",
	}
	forceSessionTimeZone(params)
	want := map[string]string{"timezone": SessionTimeZone, "application_name": "relayguard"}
	if !reflect.DeepEqual(params, want) {
		t.Fatalf("params = %v, want %v", params, want)
	}
}

// The real parser path: a URL that sets TimeZone ends up with exactly one
// timezone key, UTC. (TestDBConnectForcesUTC in internal/dbtest checks the
// live session.)
func TestForceSessionTimeZoneOnParsedURL(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://u:p@localhost:5433/db?sslmode=disable&TimeZone=Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	forceSessionTimeZone(cfg.ConnConfig.RuntimeParams)
	n := 0
	for k, v := range cfg.ConnConfig.RuntimeParams {
		if k == "timezone" && v == SessionTimeZone {
			n++
		} else if k == "TimeZone" || k == "timezone" {
			t.Errorf("leftover runtime param %s=%s", k, v)
		}
	}
	if n != 1 {
		t.Fatalf("runtime params %v, want exactly one timezone=%s", cfg.ConnConfig.RuntimeParams, SessionTimeZone)
	}
}
