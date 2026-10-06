package umami

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetric(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    metric
		wantErr bool
	}{
		{"v3 number", `812`, 812, false},
		{"v2 object", `{"value":812,"prev":745}`, 812, false},
		{"v2 object with spaces", ` { "prev": 1, "value": 5 } `, 5, false},
		{"numeric string", `"7"`, 7, false},
		{"float is rounded", `3.6`, 4, false},
		{"float in an object", `{"value":2.4}`, 2, false},
		{"zero", `0`, 0, false},
		{"null", `null`, 0, false},
		{"null value", `{"value":null}`, 0, false},
		{"empty object", `{}`, 0, false},
		{"empty string", `""`, 0, false},
		{"text", `"abc"`, 0, true},
		{"not a number", `"NaN"`, 0, true},
		{"infinite", `"Inf"`, 0, true},
		{"array", `[1]`, 0, true},
		{"object holding an object", `{"value":{"value":1}}`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got metric
			err := json.Unmarshal([]byte(tt.in), &got)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestStatsIgnoreUnknownFields(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want stats
	}{
		{
			"v2",
			`{"pageviews":{"value":2210,"prev":2054},"visitors":{"value":498,"prev":455},"visits":{"value":812,"prev":745},"bounces":{"value":398,"prev":371},"totaltime":{"value":104220,"prev":96310}}`,
			stats{Visits: 812, Pageviews: 2210},
		},
		{
			"v3 with comparison",
			`{"pageviews":2210,"visitors":498,"visits":812,"bounces":398,"totaltime":104220,"comparison":{"pageviews":2054,"visitors":455,"visits":745,"bounces":371,"totaltime":96310}}`,
			stats{Visits: 812, Pageviews: 2210},
		},
		{"visits only", `{"visits":3}`, stats{Visits: 3}},
		{"empty", `{}`, stats{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got stats
			require.NoError(t, json.Unmarshal([]byte(tt.in), &got))
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestActive(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    active
		wantErr bool
	}{
		{"v2 array", `[{"x":3}]`, 3, false},
		{"object with x", `{"x":2}`, 2, false},
		{"v3 visitors", `{"visitors":4}`, 4, false},
		{"visitors win over x", `{"visitors":4,"x":9}`, 4, false},
		{"array of visitors", `[{"visitors":5}]`, 5, false},
		{"first item only", `[{"x":1},{"x":8}]`, 1, false},
		{"empty array", `[]`, 0, false},
		{"empty object", `{}`, 0, false},
		{"bare number", `3`, 3, false},
		{"null", `null`, 0, false},
		{"text", `"x"`, 0, true},
		{"array of numbers", `[3]`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got active
			err := json.Unmarshal([]byte(tt.in), &got)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestWebsiteList(t *testing.T) {
	one := website{ID: "a", Name: "A", Domain: "a.example"}
	tests := []struct {
		name string
		in   string
		want websiteList
	}{
		{"bare array", `[{"id":"a","name":"A","domain":"a.example","shareId":null}]`, websiteList{one}},
		{"data envelope", `{"data":[{"id":"a","name":"A","domain":"a.example"}],"count":1,"page":1,"pageSize":200}`, websiteList{one}},
		{"padded array", "\n [ {\"id\":\"a\",\"name\":\"A\",\"domain\":\"a.example\"} ] \n", websiteList{one}},
		{"empty array", `[]`, websiteList{}},
		{"empty data", `{"data":[]}`, websiteList{}},
		{"no data", `{}`, nil},
		{"null", `null`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got websiteList
			require.NoError(t, json.Unmarshal([]byte(tt.in), &got))
			assert.Equal(t, tt.want, got)
		})
	}
	var bad websiteList
	require.Error(t, json.Unmarshal([]byte(`"nope"`), &bad))
}

func TestBucketDay(t *testing.T) {
	ms := func(s string) string {
		ts, err := time.Parse(time.RFC3339, s)
		require.NoError(t, err)
		return strconv.FormatInt(ts.UnixMilli(), 10)
	}
	tests := []struct {
		name string
		x    string
		want string
		ok   bool
	}{
		{"naive local time", `"2026-10-25 00:00:00"`, "2026-10-25", true},
		{"naive with T", `"2026-10-25T00:00:00"`, "2026-10-25", true},
		{"date only", `"2026-10-25"`, "2026-10-25", true},
		{"UTC midnight label", `"2026-10-25T00:00:00Z"`, "2026-10-25", true},
		{"UTC midnight label with milliseconds", `"2026-10-25T00:00:00.000Z"`, "2026-10-25", true},
		{"CEST midnight as a true instant", `"2026-10-24T22:00:00.000Z"`, "2026-10-25", true},
		{"CET midnight as a true instant", `"2026-10-25T23:00:00Z"`, "2026-10-26", true},
		{"CET midnight with an offset", `"2026-10-26T00:00:00+01:00"`, "2026-10-26", true},
		{"CEST midnight with an offset", `"2026-10-25T00:00:00+02:00"`, "2026-10-25", true},
		{"unix ms, CEST midnight", ms("2026-10-24T22:00:00Z"), "2026-10-25", true},
		{"unix ms, CET midnight", ms("2026-10-25T23:00:00Z"), "2026-10-26", true},
		{"unix ms, UTC midnight label", ms("2026-10-25T00:00:00Z"), "2026-10-25", true},
		{"unix ms, mid-day", ms("2026-10-25T10:30:00Z"), "2026-10-25", true},
		{"unix ms, evening wraps to the next local day", ms("2026-10-25T23:30:00Z"), "2026-10-26", true},
		{"null", `null`, "", false},
		{"text", `"yesterday"`, "", false},
		{"boolean", `true`, "", false},
		{"object", `{}`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := bucket{X: json.RawMessage(tt.x)}.day(paris)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
	_, ok := bucket{}.day(paris)
	assert.False(t, ok, "missing x")
}

func TestBucketDayWestOfUTC(t *testing.T) {
	newYork := mustLocation("America/New_York")
	tests := []struct {
		name string
		x    string
		want string
	}{
		{"UTC midnight label stays on its date", `"2026-10-25T00:00:00.000Z"`, "2026-10-25"},
		{"true instant of local midnight", `"2026-10-25T04:00:00.000Z"`, "2026-10-25"},
		{"true instant after the DST change", `"2026-11-02T05:00:00.000Z"`, "2026-11-02"},
		{"naive local time", `"2026-10-25 00:00:00"`, "2026-10-25"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := bucket{X: json.RawMessage(tt.x)}.day(newYork)
			require.True(t, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPct(t *testing.T) {
	tests := []struct {
		name      string
		cur, prev int
		want      *float64
	}{
		{"increase", 812, 745, ptr(9.0)},
		{"decrease", 240, 251, ptr(-4.4)},
		{"rounded to a tenth", 96, 71, ptr(35.2)},
		{"totals", 1148, 1067, ptr(7.6)},
		{"half up", 1005, 1000, ptr(0.5)},
		{"unchanged", 50, 50, ptr(0.0)},
		{"from nothing", 10, 0, nil},
		{"nothing at all", 0, 0, nil},
		{"drops to zero", 0, 40, ptr(-100.0)},
		{"doubles", 20, 10, ptr(100.0)},
		{"tiny decrease rounds to zero", 99999, 100000, ptr(0.0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pct(tt.cur, tt.prev))
		})
	}
}

func TestPctNeverProducesNegativeZero(t *testing.T) {
	b, err := json.Marshal(pct(99999, 100000))
	require.NoError(t, err)
	assert.Equal(t, "0", string(b))
}

func TestGroupDigits(t *testing.T) {
	for in, want := range map[int]string{0: "0", 7: "7", 999: "999", 1000: "1 000", 1148: "1 148", 1234567: "1 234 567", -1500: "-1 500", -12: "-12"} {
		assert.Equal(t, want, groupDigits(in))
	}
}
