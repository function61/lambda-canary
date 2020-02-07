package alertmanagertypes

import (
	"fmt"
	"time"
)

type Alert struct {
	// will be filled at ingestion time with sequential number: 1, 2, 3, ...
	// this is to implement race condition -free rate limiting by utilizing unique column constraint on save
	Key       string    `json:"alert_key"`
	Subject   string    `json:"subject"`
	Details   string    `json:"details"`
	Timestamp time.Time `json:"timestamp"`
}

func (a *Alert) Equal(other Alert) bool {
	return a.Subject == other.Subject
}

type DeadMansSwitch struct {
	Subject string    `json:"subject"`
	TTL     time.Time `json:"ttl"`
}

func (d *DeadMansSwitch) AsAlert(now time.Time) Alert {
	return Alert{
		Subject:   d.Subject,
		Timestamp: now,
		Details:   fmt.Sprintf("Check-in late by %s (%s)", now.Sub(d.TTL), d.TTL.Format(time.RFC3339Nano)),
	}
}

// otherwise the same but TTL in un-expanded form
type DeadMansSwitchCheckinRequest struct {
	Subject string `json:"subject"`
	TTL     string `json:"ttl"`
}

func (d *DeadMansSwitchCheckinRequest) AsAlert(details string) Alert {
	return Alert{
		Subject:   d.Subject,
		Details:   details,
		Timestamp: time.Now(),
	}
}

func NewDeadMansSwitchCheckinRequest(subject string, ttl string) DeadMansSwitchCheckinRequest {
	return DeadMansSwitchCheckinRequest{
		Subject: subject,
		TTL:     ttl,
	}
}
