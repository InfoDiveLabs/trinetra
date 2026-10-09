// Package trinetra: coreapi_alerts_test.go pins core.AlertRecord's
// AckedAt/Title/Delivered field mapping.
package trinetra

import (
	"testing"
)

// TestActiveAlertRecordsMapsAckedAtNotTitleOrDelivered exercises activeAlertRecords
// directly against a seeded AlertState.
func TestActiveAlertRecordsMapsAckedAtNotTitleOrDelivered(t *testing.T) {
	state := NewAlertState()
	state.Active["cpu"] = ActiveAlert{Since: 1000, Reason: "cpu = 95.0 >= threshold 90.0", Critical: true, Acked: false}
	state.Active["mem"] = ActiveAlert{Since: 2000, Reason: "mem = 80.0 >= threshold 75.0", Critical: false, Acked: true, AckedAt: 2500}

	got := activeAlertRecords(state)
	byKey := map[string]int{}
	for i, r := range got {
		byKey[r.Key] = i
	}

	cpu := got[byKey["cpu"]]
	if cpu.AckedAt != 0 {
		t.Errorf("cpu AckedAt = %d, want 0 (not acked)", cpu.AckedAt)
	}
	if cpu.Title != "" {
		t.Errorf("cpu Title = %q, want empty (ActiveAlert has no title field)", cpu.Title)
	}
	if cpu.Delivered != false {
		t.Errorf("cpu Delivered = %v, want false (ActiveAlert has no delivery field)", cpu.Delivered)
	}

	mem := got[byKey["mem"]]
	if mem.AckedAt != 2500 {
		t.Errorf("mem AckedAt = %d, want 2500 (ActiveAlert.AckedAt)", mem.AckedAt)
	}
	if mem.Title != "" {
		t.Errorf("mem Title = %q, want empty (ActiveAlert has no title field)", mem.Title)
	}
	if mem.Delivered != false {
		t.Errorf("mem Delivered = %v, want false (ActiveAlert has no delivery field)", mem.Delivered)
	}
}

// TestAlertHistoryRecordsMapsTitleAndDelivered exercises alertHistoryRecords directly
// against a seeded AlertLog: Title is a direct copy of AlertEvent.Title.
func TestAlertHistoryRecordsMapsTitleAndDelivered(t *testing.T) {
	dir := t.TempDir()
	log := NewAlertLog(dir + "/alertlog.jsonl")

	events := []AlertEvent{
		{Time: 100, Key: "cpu", Title: "CPU high", Severity: "critical", Kind: "fire", Source: "threshold",
			Delivered: []Delivery{{Channel: "email", OK: true}}},
		{Time: 200, Key: "mem", Title: "Mem high", Severity: "warning", Kind: "fire", Source: "threshold",
			Delivered: []Delivery{{Channel: "email", OK: false, Err: "smtp timeout"}}},
		{Time: 300, Key: "disk", Title: "Disk high", Severity: "warning", Kind: "fire", Source: "threshold"},
	}
	for _, ev := range events {
		if err := log.AppendAlertEvent(ev); err != nil {
			t.Fatalf("AppendAlertEvent(%+v): %v", ev, err)
		}
	}

	got, err := alertHistoryRecords(log, 0, 0)
	if err != nil {
		t.Fatalf("alertHistoryRecords: %v", err)
	}
	byKey := map[string]int{}
	for i, r := range got {
		byKey[r.Key] = i
	}

	cpu := got[byKey["cpu"]]
	if cpu.Title != "CPU high" {
		t.Errorf("cpu Title = %q, want %q", cpu.Title, "CPU high")
	}
	if cpu.Delivered != true {
		t.Errorf("cpu Delivered = %v, want true (one successful delivery)", cpu.Delivered)
	}
	if cpu.AckedAt != 0 {
		t.Errorf("cpu AckedAt = %d, want 0 (AlertEvent has no ack timestamp)", cpu.AckedAt)
	}

	mem := got[byKey["mem"]]
	if mem.Title != "Mem high" {
		t.Errorf("mem Title = %q, want %q", mem.Title, "Mem high")
	}
	if mem.Delivered != false {
		t.Errorf("mem Delivered = %v, want false (only delivery attempt failed)", mem.Delivered)
	}

	disk := got[byKey["disk"]]
	if disk.Title != "Disk high" {
		t.Errorf("disk Title = %q, want %q", disk.Title, "Disk high")
	}
	if disk.Delivered != false {
		t.Errorf("disk Delivered = %v, want false (no delivery recorded)", disk.Delivered)
	}
}

// TestAlertHistoryRecordsMapsDeliveredChannels pins the DeliveredTo field: the names of the
// channels that actually accepted an AlertEvent's delivery (Delivery.OK true).
func TestAlertHistoryRecordsMapsDeliveredChannels(t *testing.T) {
	dir := t.TempDir()
	log := NewAlertLog(dir + "/alertlog.jsonl")

	events := []AlertEvent{
		{Time: 100, Key: "cpu", Title: "CPU high", Severity: "critical", Kind: "fire", Source: "threshold",
			Delivered: []Delivery{{Channel: "telegram", OK: true}, {Channel: "email", OK: false, Err: "smtp timeout"}}},
		{Time: 200, Key: "mem", Title: "Mem high", Severity: "warning", Kind: "fire", Source: "threshold",
			Delivered: []Delivery{{Channel: "email", OK: false, Err: "smtp timeout"}}},
		{Time: 300, Key: "disk", Title: "Disk high", Severity: "warning", Kind: "fire", Source: "threshold"},
	}
	for _, ev := range events {
		if err := log.AppendAlertEvent(ev); err != nil {
			t.Fatalf("AppendAlertEvent(%+v): %v", ev, err)
		}
	}

	got, err := alertHistoryRecords(log, 0, 0)
	if err != nil {
		t.Fatalf("alertHistoryRecords: %v", err)
	}
	byKey := map[string]int{}
	for i, r := range got {
		byKey[r.Key] = i
	}

	cpu := got[byKey["cpu"]]
	if len(cpu.DeliveredTo) != 1 || cpu.DeliveredTo[0] != "telegram" {
		t.Errorf("cpu DeliveredTo = %v, want [telegram] (only the OK channel)", cpu.DeliveredTo)
	}

	mem := got[byKey["mem"]]
	if len(mem.DeliveredTo) != 0 {
		t.Errorf("mem DeliveredTo = %v, want empty (its only delivery attempt failed)", mem.DeliveredTo)
	}

	disk := got[byKey["disk"]]
	if len(disk.DeliveredTo) != 0 {
		t.Errorf("disk DeliveredTo = %v, want empty (no delivery recorded)", disk.DeliveredTo)
	}
}
