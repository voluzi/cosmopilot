package chainnode

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAuthoritativeSDKHaltLog(t *testing.T) {
	const logTimestamp = "2026-09-21T12:00:30.123456789Z"
	const validJSON = logTimestamp + ` {"level":"info","height":100,"time":0,"_msg":"halting node per configuration"}` + "\n"
	const validPlain = logTimestamp + " I[2026-09-21|12:00:30.123] halting node per configuration               height=100 time=0\n"
	startedAt := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(time.Minute)

	for _, tt := range []struct {
		name string
		logs string
		want bool
	}{
		{name: "json halt-height evidence", logs: validJSON, want: true},
		{name: "zerolog json halt-height evidence", logs: logTimestamp + ` {"level":"info","time":"1:23PM","height":100,"time":0,"message":"halting node per configuration"}` + "\n", want: true},
		{name: "plain halt-height evidence", logs: validPlain, want: true},
		{name: "zerolog plain halt-height evidence", logs: logTimestamp + " 1:23PM INF halting node per configuration height=100 time=0\n", want: true},
		{name: "cached tail may contain unrelated complete lines", logs: "2026-09-21T12:00:20Z I[2026-09-21|12:00:20.000] committed block height=99\n" + validPlain, want: true},
		{name: "halt-time exit is not halt-height evidence", logs: logTimestamp + ` {"level":"info","height":100,"time":1700000000,"_msg":"halting node per configuration"}` + "\n"},
		{name: "direct clean termination has no halt evidence", logs: logTimestamp + " I[2026-09-21|12:00:30.123] caught signal signal=terminated\n"},
		{name: "wrong height", logs: logTimestamp + ` {"level":"info","height":99,"time":0,"_msg":"halting node per configuration"}` + "\n"},
		{name: "malformed message line", logs: logTimestamp + ` {"height":100,"time":0,"_msg":"halting node per configuration"` + "\n"},
		{name: "oversized tail", logs: strings.Repeat("x", appTerminationLogMaxBytes+1)},
		{name: "truncated matching line", logs: strings.TrimSuffix(validJSON, "\n")},
		{name: "record before container start", logs: "2026-09-21T11:59:59Z " + strings.SplitN(validJSON, " ", 2)[1]},
		{name: "record after container finish", logs: "2026-09-21T12:01:01Z " + strings.SplitN(validJSON, " ", 2)[1]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasAuthoritativeHaltLog([]byte(tt.logs), 100, startedAt, finishedAt); got != tt.want {
				t.Fatalf("hasAuthoritativeHaltLog() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAuthoritativeSDKHaltLogAllowsKubernetesFinishedAtPrecisionLoss(t *testing.T) {
	startedAt := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	original := metav1.NewTime(time.Date(2026, 9, 21, 12, 1, 0, 987654321, time.UTC))
	body, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var serialized metav1.Time
	if err := json.Unmarshal(body, &serialized); err != nil {
		t.Fatal(err)
	}
	if !serialized.Time.Equal(time.Date(2026, 9, 21, 12, 1, 0, 0, time.UTC)) {
		t.Fatalf("serialized finishedAt retained unexpected precision: %s", serialized.Time)
	}

	for _, tt := range []struct {
		name       string
		recordedAt string
		want       bool
	}{
		{name: "fractional second after serialized finish is accepted", recordedAt: "2026-09-21T12:01:00.987654321Z", want: true},
		{name: "exactly one second after serialized finish is rejected", recordedAt: "2026-09-21T12:01:01Z"},
		{name: "more than one second after serialized finish is rejected", recordedAt: "2026-09-21T12:01:01.000000001Z"},
		{name: "before container start is rejected", recordedAt: "2026-09-21T11:59:59.999999999Z"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logs := tt.recordedAt + ` {"level":"info","height":100,"time":0,"_msg":"halting node per configuration"}` + "\n"
			if got := hasAuthoritativeHaltLog([]byte(logs), 100, startedAt, serialized.Time); got != tt.want {
				t.Fatalf("hasAuthoritativeHaltLog() = %v, want %v", got, tt.want)
			}
		})
	}
}
