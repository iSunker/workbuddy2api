package cred

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestLoadFileFlatAPIKey(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "codebuddy-u1.json",
		`{"api_key":"ck_abcdefghijklmn","uid":"u1","nickname":"Alice"}`)
	c, err := LoadFile(f)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if c.Token != "ck_abcdefghijklmn" || c.UID != "u1" || c.Nickname != "Alice" {
		t.Fatalf("unexpected: %+v", c)
	}
	if c.Kind != KindAPIKey {
		t.Fatalf("kind = %v, want api_key", c.Kind)
	}
}

func TestLoadFileNestedIDEStyle(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "a.json",
		`{"auth":{"accessToken":"eyJhbGciOi","refreshToken":"rt_1","expiresAt":1700000000},
		  "account":{"uid":"u2","nickname":"Bob"}}`)
	c, err := LoadFile(f)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if c.Token != "eyJhbGciOi" || c.RefreshToken != "rt_1" || c.ExpiresAt != 1700000000 {
		t.Fatalf("unexpected tokens: %+v", c)
	}
	if c.UID != "u2" || c.Nickname != "Bob" {
		t.Fatalf("unexpected account: %+v", c)
	}
	if c.Kind != KindToken {
		t.Fatalf("kind = %v, want token", c.Kind)
	}
}

func TestLoadFileAliases(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "b.json", `{"key":"ck_zzz","user_id":"u3"}`)
	c, err := LoadFile(f)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if c.Token != "ck_zzz" || c.UID != "u3" {
		t.Fatalf("unexpected: %+v", c)
	}
}

func TestLoadFileUIDFallback(t *testing.T) {
	dir := t.TempDir()
	// 无 uid → 从文件名 codebuddy-<uid>.json 提取
	f := write(t, dir, "codebuddy-fromname.json", `{"api_key":"ck_x1"}`)
	c, err := LoadFile(f)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if c.UID != "fromname" {
		t.Fatalf("uid = %q, want fromname", c.UID)
	}

	// 连文件名也没有 → 用 token 指纹，保证稳定
	f2 := write(t, dir, "noname.json", `{"api_key":"ck_x2"}`)
	c2, err := LoadFile(f2)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if c2.UID == "" || c2.UID == "noname" {
		t.Fatalf("uid should be derived from token fingerprint, got %q", c2.UID)
	}
	c3, _ := LoadFile(f2)
	if c2.UID != c3.UID {
		t.Fatal("fingerprint must be stable across loads")
	}
	if strings.Contains(c2.Nickname, "ck_x2") {
		t.Fatalf("nickname leaked the raw token: %q", c2.Nickname)
	}
}

func TestLoadFileMissingToken(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "bad.json", `{"uid":"u1"}`)
	if _, err := LoadFile(f); err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestLoadDirSkipsBroken(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "good.json", `{"api_key":"ck_ok1","uid":"g1"}`)
	write(t, dir, "broken.json", `{not json`)
	write(t, dir, "note.txt", "ignore me")

	creds, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(creds) != 1 || creds[0].UID != "g1" {
		t.Fatalf("expected only the valid credential, got %+v", creds)
	}
}

func TestSaveFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := credFixture()
	path, err := SaveFile(dir, c)
	if err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(path), "codebuddy-") {
		t.Fatalf("unexpected file name: %s", path)
	}
	back, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if back.Token != c.Token || back.UID != c.UID || back.Nickname != c.Nickname {
		t.Fatalf("round trip mismatch: %+v vs %+v", back, c)
	}

	raw, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["api_key"] != c.Token {
		t.Fatalf("api_key not persisted: %v", doc)
	}
	if _, ok := doc["token"]; ok {
		t.Fatal("api_key credentials must not also write a token field")
	}
}

func credFixture() *Cred {
	return &Cred{UID: "save-1", Nickname: "saved", Token: "ck_saved", Kind: KindAPIKey}
}

func TestEnsureTokenAPIKeyNeverRefreshes(t *testing.T) {
	c := &Cred{UID: "u", Token: "ck_static", Kind: KindAPIKey, ExpiresAt: 0}
	tok, err := c.EnsureToken("https://example.invalid/oauth2/token", "")
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if tok != "ck_static" {
		t.Fatalf("token changed: %q", tok)
	}
}

func TestEnsureTokenExpiredWithoutRefreshTokenStillReturns(t *testing.T) {
	// 过期但无 refresh_token：先放行一次，交给上游 401 判定，避免误禁用
	c := &Cred{UID: "u", Token: "expired-jwt", Kind: KindToken, ExpiresAt: 1}
	tok, err := c.EnsureToken("https://example.invalid/oauth2/token", "")
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if tok != "expired-jwt" {
		t.Fatalf("token = %q", tok)
	}
}

func TestClassifyKind(t *testing.T) {
	if ClassifyKind("ck_abc") != KindAPIKey {
		t.Fatal("ck_ prefix should be api_key")
	}
	if ClassifyKind("eyJhbGciOi...") != KindToken {
		t.Fatal("jwt should be token")
	}
}

func TestMaskToken(t *testing.T) {
	if got := maskToken("ck_abcdefghijklmnop"); got != "ck_abc***mnop" {
		t.Fatalf("maskToken = %q", got)
	}
	if got := maskToken("short"); got != "***" {
		t.Fatalf("maskToken(short) = %q", got)
	}
}
