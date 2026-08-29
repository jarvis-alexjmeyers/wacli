package wa

import (
	"context"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
)

func TestPersistentLIDMappingPreservesRecipientDevice(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "session.db") + "?_foreign_keys=on"

	store, err := sqlstore.New(ctx, "sqlite3", dsn, nil)
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	pn := types.NewJID("15551234567", types.DefaultUserServer)
	lid := types.NewJID("123456789012345", types.HiddenUserServer)
	if err := store.LIDMap.PutLIDMapping(ctx, lid, pn); err != nil {
		t.Fatalf("persist LID mapping: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close session store: %v", err)
	}

	store, err = sqlstore.New(ctx, "sqlite3", dsn, nil)
	if err != nil {
		t.Fatalf("reopen session store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	recipientDevice := types.NewADJID(pn.User, types.WhatsAppDomain, 19)
	mappings, err := store.LIDMap.GetManyLIDsForPNs(ctx, []types.JID{recipientDevice})
	if err != nil {
		t.Fatalf("read LID mapping: %v", err)
	}
	got, ok := mappings[recipientDevice]
	if !ok {
		t.Fatal("recipient device mapping missing")
	}
	if got.User != lid.User || got.Server != types.HiddenUserServer || got.Device != recipientDevice.Device {
		t.Fatalf("mapped device = %s, want LID user with device %d", got, recipientDevice.Device)
	}
}
