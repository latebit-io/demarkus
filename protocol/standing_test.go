package protocol

import "testing"

func TestStandingOf(t *testing.T) {
	tests := []struct {
		tags []string
		want Standing
	}{
		{nil, StandingProvisional},
		{[]string{"adr"}, StandingProvisional},
		{[]string{"status:draft"}, StandingProvisional},
		{[]string{"status:wip"}, StandingProvisional},
		{[]string{"status:acepted"}, StandingProvisional},
		{[]string{"status:accepted"}, StandingAccepted},
		{[]string{"Status: Accepted "}, StandingAccepted},
		{[]string{"status:archived"}, StandingRetired},
		{[]string{"status:superseded"}, StandingRetired},
		{[]string{"status:stable"}, StandingAccepted},
		{[]string{"status:deprecated"}, StandingRetired},
		{[]string{"status:wip", "status:accepted"}, StandingProvisional}, // first status tag wins
		{[]string{"statusaccepted", "status"}, StandingProvisional},
	}
	for _, tt := range tests {
		if got := StandingOf(tt.tags); got != tt.want {
			t.Errorf("StandingOf(%q) = %d, want %d", tt.tags, got, tt.want)
		}
	}
}

func TestStandingOfStatus(t *testing.T) {
	if StandingOfStatus(" Stable ") != StandingAccepted || StandingOfStatus("deprecated") != StandingRetired || StandingOfStatus("") != StandingProvisional {
		t.Error("StandingOfStatus does not trim, fold case, or default to provisional")
	}
}
