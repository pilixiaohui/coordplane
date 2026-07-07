package objects_test

import (
	"context"
	"database/sql"
	"testing"

	"coordplane/internal/capability"
	"coordplane/internal/objects"
	"coordplane/internal/store"

	_ "modernc.org/sqlite"
)

func TestObjectStoreContentIsImmutableAndReadableByRef(t *testing.T) {
	ctx := context.Background()
	objStore, db := newObjectStore(t)

	first, err := objStore.Put(ctx, objects.PutInput{
		OwnerAgent:  "builder",
		Content:     []byte("sensitive report body"),
		ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("put first object: %v", err)
	}
	duplicate, err := objStore.Put(ctx, objects.PutInput{
		OwnerAgent:  "builder",
		Content:     []byte("sensitive report body"),
		ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("put duplicate object: %v", err)
	}
	if duplicate.Ref != first.Ref || duplicate.Checksum != first.Checksum {
		t.Fatalf("duplicate object = %+v, want same ref/checksum as %+v", duplicate, first)
	}
	changed, err := objStore.Put(ctx, objects.PutInput{
		OwnerAgent:  "builder",
		Content:     []byte("changed report body"),
		ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("put changed object: %v", err)
	}
	if changed.Ref == first.Ref {
		t.Fatalf("changed content reused ref %s", changed.Ref)
	}
	if got := countRows(t, ctx, db, "object_blobs"); got != 2 {
		t.Fatalf("object rows = %d, want 2 immutable refs", got)
	}

	read := objStore.Read(ctx, agentSubject("builder"), first.Ref)
	if read.Status != capability.StatusAccepted || read.Data == nil {
		t.Fatalf("object.read = %+v, want accepted", read)
	}
	if read.Data.Content != "sensitive report body" {
		t.Fatalf("read content = %q, want original body", read.Data.Content)
	}
	inspect := objStore.Inspect(ctx, agentSubject("builder"), first.Ref)
	if inspect.Status != capability.StatusAccepted || inspect.Data == nil {
		t.Fatalf("object.inspect = %+v, want accepted", inspect)
	}
	if inspect.Data.SizeBytes != int64(len("sensitive report body")) || inspect.Data.Checksum != first.Checksum {
		t.Fatalf("inspect meta = %+v, want size/checksum from first object", inspect.Data)
	}
}

func TestObjectStorePersistsNilContentAsEmptyObject(t *testing.T) {
	ctx := context.Background()
	objStore, db := newObjectStore(t)

	empty, err := objStore.Put(ctx, objects.PutInput{
		OwnerAgent:  "builder",
		ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("put nil content object: %v", err)
	}
	if empty.SizeBytes != 0 {
		t.Fatalf("empty object size = %d, want 0", empty.SizeBytes)
	}
	read := objStore.Read(ctx, agentSubject("builder"), empty.Ref)
	if read.Status != capability.StatusAccepted || read.Data == nil || read.Data.Content != "" {
		t.Fatalf("object.read empty = %+v, want accepted empty content", read)
	}
	duplicate, err := objStore.Put(ctx, objects.PutInput{
		OwnerAgent:  "builder",
		Content:     []byte{},
		ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("put explicit empty content object: %v", err)
	}
	if duplicate.Ref != empty.Ref {
		t.Fatalf("explicit empty ref = %s, want existing empty ref %s", duplicate.Ref, empty.Ref)
	}
	if got := countRows(t, ctx, db, "object_blobs"); got != 1 {
		t.Fatalf("object rows = %d, want one immutable empty object", got)
	}
}

func TestObjectStoreArtifactPermissionAndTypedErrors(t *testing.T) {
	ctx := context.Background()
	objStore, _ := newObjectStore(t)
	artifact, err := objStore.PutArtifact(ctx, objects.PutArtifactInput{
		OwnerAgent:  "builder",
		Content:     []byte("artifact secret content"),
		ContentType: "text/plain",
		Metadata:    map[string]string{"name": "result.txt"},
	})
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}

	builderRead := objStore.Read(ctx, agentSubject("builder"), artifact.ObjectRef)
	if builderRead.Status != capability.StatusAccepted || builderRead.Data == nil || builderRead.Data.Content != "artifact secret content" {
		t.Fatalf("builder read artifact = %+v, want content", builderRead)
	}
	intruder := objStore.Read(ctx, agentSubject("intruder"), artifact.ObjectRef)
	if intruder.Status != capability.StatusRejected || intruder.ErrorCode != "OBJECT_ACCESS_DENIED" {
		t.Fatalf("intruder read = %+v, want OBJECT_ACCESS_DENIED", intruder)
	}
	missing := objStore.Inspect(ctx, agentSubject("builder"), "obj_sha256_missing")
	if missing.Status != capability.StatusRejected || missing.ErrorCode != "OBJECT_NOT_FOUND" {
		t.Fatalf("missing inspect = %+v, want OBJECT_NOT_FOUND", missing)
	}
}

func newObjectStore(t *testing.T) (*objects.Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_ = db.Close()
	})
	st := store.New(db)
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return objects.NewStore(st), db
}

func agentSubject(agentID string) capability.Subject {
	return capability.Subject{Kind: "agent", ID: agentID, AgentID: agentID}
}

func countRows(t *testing.T, ctx context.Context, db *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}
