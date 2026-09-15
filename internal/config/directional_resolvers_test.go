package config

import "testing"

func TestNormalizeDirectionalResolverPercent(t *testing.T) {
	cases := []struct {
		name     string
		uplink   int
		downlink int
		wantUp   int
		wantDown int
	}{
		{name: "defaults when both unset", uplink: 0, downlink: 0, wantUp: 75, wantDown: 25},
		{name: "explicit 75/25", uplink: 75, downlink: 25, wantUp: 75, wantDown: 25},
		{name: "disabled split", uplink: 100, downlink: 0, wantUp: 100, wantDown: 0},
		{name: "all downlink derives uplink", uplink: 0, downlink: 100, wantUp: 0, wantDown: 100},
		{name: "normalizes when sum differs", uplink: 60, downlink: 60, wantUp: 50, wantDown: 50},
		{name: "clamps out of range", uplink: 200, downlink: 200, wantUp: 50, wantDown: 50},
		{name: "negative clamps to zero", uplink: -10, downlink: -10, wantUp: 75, wantDown: 25},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, down := normalizeDirectionalResolverPercent(tc.uplink, tc.downlink)
			if up != tc.wantUp || down != tc.wantDown {
				t.Fatalf("normalizeDirectionalResolverPercent(%d, %d) = (%d, %d), want (%d, %d)",
					tc.uplink, tc.downlink, up, down, tc.wantUp, tc.wantDown)
			}
			if up+down != 100 {
				t.Fatalf("expected percentages to add up to 100, got %d + %d", up, down)
			}
		})
	}
}
