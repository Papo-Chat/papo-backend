package storage

import (
    "errors"
    "testing"

    "papo/internal/models"
)

// Initial relationships must be committed along with their owning object.
func TestAtomicChannelCreation(t *testing.T) {
    _ = newTestServer(t, nil)
    category, err := CreateChannel(testCtx(), "category_"+randHex(7), "category", "")
    if err != nil { t.Fatal(err) }
    role, err := CreateRole(testCtx(), "role_"+randHex(7), nil, models.RolePermissions{})
    if err != nil { t.Fatal(err) }

    channelName := "restricted_"+randHex(7)
    created, err := CreateChannelWithOptions(testCtx(), channelName, "text", "", &category.ID, []string{role.ID})
    if err != nil { t.Fatal(err) }
    if created.ParentID == nil || *created.ParentID != category.ID { t.Fatalf("parent not saved: %+v", created.ParentID) }
    if !created.Permissions[role.ID].ReadChannel { t.Fatalf("permissions not saved atomically: %+v", created.Permissions) }
    summary, err := GetChannelSummary(testCtx(), created.ID)
    if err != nil { t.Fatal(err) }
    if summary.ParentID == nil || *summary.ParentID != category.ID || len(summary.Permissions) != 1 {
        t.Fatalf("summary lacks initial configuration: %+v", summary)
    }

    invalidName := "invalid_"+randHex(7)
    _, err = CreateChannelWithOptions(testCtx(), invalidName, "text", "", &category.ID, []string{randUUID()})
    if !errors.Is(err, ErrInitialChannelRoleNotFound) { t.Fatalf("want invalid role, got %v", err) }
    var n int
    if err := GetDB().QueryRowContext(testCtx(), "SELECT count(*) FROM channels WHERE name = $1", invalidName).Scan(&n); err != nil { t.Fatal(err) }
    if n != 0 { t.Fatal("invalid request created a public channel") }

    nonCategory := created.ID
    _, err = CreateChannelWithOptions(testCtx(), "invalid_parent_"+randHex(5), "text", "", &nonCategory, nil)
    if !errors.Is(err, ErrCategoryNotFound) { t.Fatalf("want invalid category, got %v", err) }
    unparent := ""
    moved, err := ChangeChannelPositionWithParent(testCtx(), created.ID, created.Position, created.Position, &unparent)
    if err != nil { t.Fatal(err) }
    if moved.ParentID != nil { t.Fatalf("category not removed: %+v", moved.ParentID) }
}

func TestAtomicRoleCreation(t *testing.T) {
    _ = newTestServer(t, nil)
    user := newTestUser(t)
    roleName := "assigned_"+randHex(8)
    role, err := CreateRoleWithMembers(testCtx(), roleName, nil, models.RolePermissions{}, []string{user.ID, user.ID})
    if err != nil { t.Fatal(err) }
    assignments, err := GetUsersByRole(testCtx(), role.ID)
    if err != nil { t.Fatal(err) }
    if len(assignments) != 1 || assignments[0] != user.ID { t.Fatalf("wrong initial members: %v", assignments) }

    badName := "failed_"+randHex(8)
    _, err = CreateRoleWithMembers(testCtx(), badName, nil, models.RolePermissions{}, []string{user.ID, randUUID()})
    if !errors.Is(err, ErrNotFound) { t.Fatalf("want missing user, got %v", err) }
    var count int
    if err := GetDB().QueryRowContext(testCtx(), "SELECT count(*) FROM roles WHERE name = $1", badName).Scan(&count); err != nil { t.Fatal(err) }
    if count != 0 { t.Fatal("failed role creation was not rolled back") }
}
