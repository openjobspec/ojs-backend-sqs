package sqs

import (
	"strconv"
	"testing"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
)

func TestBuildMessageAttributes_RequiredFields(t *testing.T) {
	job := &core.Job{
		ID:      "job-123",
		Type:    "email.send",
		Queue:   "default",
		Attempt: 0,
	}

	attrs := BuildMessageAttributes(job)

	tests := []struct {
		key      string
		expected string
	}{
		{AttrOJSSpecVersion, core.OJSVersion},
		{AttrOJSID, "job-123"},
		{AttrOJSType, "email.send"},
		{AttrOJSQueue, "default"},
		{AttrOJSAttempt, "0"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			attr, ok := attrs[tt.key]
			if !ok {
				t.Fatalf("missing attribute %q", tt.key)
			}
			if *attr.StringValue != tt.expected {
				t.Errorf("attribute %q = %q, want %q", tt.key, *attr.StringValue, tt.expected)
			}
		})
	}
}

func TestBuildMessageAttributes_WithPriority(t *testing.T) {
	priority := 5
	job := &core.Job{
		ID:       "job-123",
		Type:     "test",
		Queue:    "default",
		Priority: &priority,
	}

	attrs := BuildMessageAttributes(job)

	attr, ok := attrs[AttrOJSPriority]
	if !ok {
		t.Fatal("missing priority attribute")
	}
	if *attr.DataType != "Number" {
		t.Errorf("priority DataType = %q, want %q", *attr.DataType, "Number")
	}
	if *attr.StringValue != strconv.Itoa(priority) {
		t.Errorf("priority value = %q, want %q", *attr.StringValue, strconv.Itoa(priority))
	}
}

func TestBuildMessageAttributes_WithoutPriority(t *testing.T) {
	job := &core.Job{
		ID:    "job-123",
		Type:  "test",
		Queue: "default",
	}

	attrs := BuildMessageAttributes(job)

	if _, ok := attrs[AttrOJSPriority]; ok {
		t.Error("expected no priority attribute when priority is nil")
	}
}

func TestBuildMessageAttributes_WithScheduledAt(t *testing.T) {
	job := &core.Job{
		ID:          "job-123",
		Type:        "test",
		Queue:       "default",
		ScheduledAt: "2025-01-01T12:00:00.000Z",
	}

	attrs := BuildMessageAttributes(job)

	attr, ok := attrs[AttrOJSScheduledAt]
	if !ok {
		t.Fatal("missing scheduled_at attribute")
	}
	if *attr.StringValue != "2025-01-01T12:00:00.000Z" {
		t.Errorf("scheduled_at = %q, want %q", *attr.StringValue, "2025-01-01T12:00:00.000Z")
	}
}

func TestBuildMessageAttributes_WithoutScheduledAt(t *testing.T) {
	job := &core.Job{
		ID:    "job-123",
		Type:  "test",
		Queue: "default",
	}

	attrs := BuildMessageAttributes(job)

	if _, ok := attrs[AttrOJSScheduledAt]; ok {
		t.Error("expected no scheduled_at attribute when empty")
	}
}

func TestBuildMessageAttributes_WithCreatedAt(t *testing.T) {
	job := &core.Job{
		ID:        "job-123",
		Type:      "test",
		Queue:     "default",
		CreatedAt: "2025-01-01T10:00:00.000Z",
	}

	attrs := BuildMessageAttributes(job)

	attr, ok := attrs[AttrOJSCreatedAt]
	if !ok {
		t.Fatal("missing created_at attribute")
	}
	if *attr.StringValue != "2025-01-01T10:00:00.000Z" {
		t.Errorf("created_at = %q, want %q", *attr.StringValue, "2025-01-01T10:00:00.000Z")
	}
}

func TestBuildMessageAttributes_WithoutCreatedAt(t *testing.T) {
	job := &core.Job{
		ID:    "job-123",
		Type:  "test",
		Queue: "default",
	}

	attrs := BuildMessageAttributes(job)

	if _, ok := attrs[AttrOJSCreatedAt]; ok {
		t.Error("expected no created_at attribute when empty")
	}
}

func TestBuildMessageAttributes_AttemptIncrement(t *testing.T) {
	tests := []struct {
		name    string
		attempt int
	}{
		{"zero attempt", 0},
		{"first attempt", 1},
		{"third attempt", 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &core.Job{
				ID:      "job-123",
				Type:    "test",
				Queue:   "default",
				Attempt: tt.attempt,
			}

			attrs := BuildMessageAttributes(job)

			attr := attrs[AttrOJSAttempt]
			if *attr.StringValue != strconv.Itoa(tt.attempt) {
				t.Errorf("attempt = %q, want %q", *attr.StringValue, strconv.Itoa(tt.attempt))
			}
		})
	}
}

func TestBuildMessageAttributes_MaxAttributeCount(t *testing.T) {
	// SQS allows max 10 attributes. Test with all optional fields set.
	priority := 1
	job := &core.Job{
		ID:          "job-123",
		Type:        "test",
		Queue:       "default",
		Priority:    &priority,
		Attempt:     2,
		ScheduledAt: "2025-01-01T12:00:00.000Z",
		CreatedAt:   "2025-01-01T10:00:00.000Z",
	}

	attrs := BuildMessageAttributes(job)

	if len(attrs) > 10 {
		t.Errorf("attribute count = %d, exceeds SQS max of 10", len(attrs))
	}
}

func TestBuildMessageAttributes_DataTypes(t *testing.T) {
	priority := 5
	job := &core.Job{
		ID:          "job-123",
		Type:        "test",
		Queue:       "default",
		Priority:    &priority,
		Attempt:     1,
		ScheduledAt: "2025-01-01T12:00:00.000Z",
		CreatedAt:   "2025-01-01T10:00:00.000Z",
	}

	attrs := BuildMessageAttributes(job)

	stringAttrs := []string{AttrOJSSpecVersion, AttrOJSID, AttrOJSType, AttrOJSQueue, AttrOJSScheduledAt, AttrOJSCreatedAt}
	for _, key := range stringAttrs {
		if attr, ok := attrs[key]; ok {
			if *attr.DataType != "String" {
				t.Errorf("attribute %q DataType = %q, want %q", key, *attr.DataType, "String")
			}
		}
	}

	numberAttrs := []string{AttrOJSPriority, AttrOJSAttempt}
	for _, key := range numberAttrs {
		if attr, ok := attrs[key]; ok {
			if *attr.DataType != "Number" {
				t.Errorf("attribute %q DataType = %q, want %q", key, *attr.DataType, "Number")
			}
		}
	}
}

func TestStrPtr(t *testing.T) {
	s := "hello"
	ptr := strPtr(s)
	if *ptr != s {
		t.Errorf("strPtr(%q) = %q, want %q", s, *ptr, s)
	}
}
