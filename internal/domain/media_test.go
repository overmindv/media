package domain

import "testing"

// TestFileAccess проверяет публичный, owner и admin доступ без обращения к storage.
func TestFileAccess(t *testing.T) {
	public := File{Visibility: VisibilityPublic, Status: StatusReady, OwnerUserID: "owner"}
	if !public.CanRead(Actor{}) {
		t.Fatal("готовый публичный файл должен читаться без actor")
	}
	private := File{Visibility: VisibilityPrivate, Status: StatusReady, OwnerUserID: "owner"}
	if private.CanRead(Actor{UserID: "other"}) {
		t.Fatal("чужой приватный файл не должен читаться")
	}
	if !private.CanRead(Actor{UserID: "owner"}) || !private.CanRead(Actor{UserID: "admin-id", Roles: []string{"admin"}}) {
		t.Fatal("owner и admin должны читать приватный файл")
	}
}
