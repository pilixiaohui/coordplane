package architecture_test

import "testing"

func TestP2LegacyCoordinationOwnersCannotReturn(t *testing.T) {
	assertNoGoFiles(t, "internal/coordination", "internal/delivery", "internal/queue")
	assertProductionTextScope(t, []string{
		"type workcontract ", "type assignment ", "type lease ", "type attempt ", "type queueitem ", "type mailbox ", "type deliveryattempt ",
	}, true, func(string) bool { return false })
	assertProductionTokenScope(t, []string{
		"work_contracts", "assignments", "leases", "queue_items", "mailbox_items", "delivery_attempts",
		"session_routes", "agent_communication_envelopes",
	}, func(string) bool { return false })
}

func TestP2ScriptedAdaptersAreExcludedFromProductionBuilds(t *testing.T) {
	assertProductionTextScope(t, []string{"scripted"}, true, func(string) bool { return false })
}
