package main

import "testing"

func TestDiscordBackend(t *testing.T) {
	// No Discord backend present.
	mn := NewMultiNotifier()
	if got := mn.DiscordBackend(); got != nil {
		t.Fatalf("expected nil DiscordBackend on empty notifier, got %v", got)
	}

	// Discord backend present (zero-value *DiscordNotifier is fine for identity).
	d := &DiscordNotifier{}
	mn2 := NewMultiNotifier(notifierBackend{notifier: d})
	if got := mn2.DiscordBackend(); got != d {
		t.Fatalf("expected DiscordBackend to return the registered *DiscordNotifier, got %v", got)
	}
}
