package config

// Secret-file tests use real files and keep secret bodies out of failures.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoad_SecretPermissionWarning covers permission warnings without leakage.
func TestLoad_SecretPermissionWarning(t *testing.T) {
	keyPath := mustWriteSecret(t, "loose-key.pem", keyPEM, 0644)
	doc := strings.ReplaceAll(baseConfigYAML, "__KEY__", keyPath)
	_, warnings, err := Load(writeConfig(t, doc))
	if err != nil {
		t.Fatal("正常な設定が拒否されました (error 本文は秘密露出のため出力しません)")
	}
	if len(warnings) != 1 {
		t.Fatalf("warning の個数が不正です: 期待値 1、実測値 %d", len(warnings))
	}
	if warnings[0].Path != "github.app.privateKeyFile" {
		t.Fatal("warning の path が不正です")
	}
	if !strings.Contains(warnings[0].Message, "set 0600") {
		t.Fatal("warning に 0600 推奨の文言がありません (warning 本文は出力しません)")
	}
	if strings.Contains(warnings[0].Message, keyPEM) {
		t.Fatal("warning に秘密の本文が含まれています")
	}

	strictKey := mustWriteSecret(t, "strict-key.pem", keyPEM, 0600)
	strictDoc := strings.ReplaceAll(baseConfigYAML, "__KEY__", strictKey)
	_, warnings, err = Load(writeConfig(t, strictDoc))
	if err != nil {
		t.Fatal("正常な設定が拒否されました (error 本文は秘密露出のため出力しません)")
	}
	if len(warnings) != 0 {
		t.Fatal("0600 で warning が返りました (warning 本文は出力しません)")
	}
}

// TestLoad_GroupOtherAnyPermissionBitWarns covers all group/other bits.
func TestLoad_GroupOtherAnyPermissionBitWarns(t *testing.T) {
	tests := []struct {
		name string
		perm os.FileMode
	}{
		{name: "group write only warns", perm: 0620},
		{name: "group execute only warns", perm: 0610},
		{name: "other write only warns", perm: 0602},
		{name: "other execute only warns", perm: 0601},
		{name: "group read only warns", perm: 0640},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyPath := mustWriteSecret(t, "perm-key.pem", keyPEM, tt.perm)
			doc := strings.ReplaceAll(baseConfigYAML, "__KEY__", keyPath)
			_, warnings, err := Load(writeConfig(t, doc))
			if err != nil {
				t.Fatal("正常な設定が拒否されました (error 本文は秘密露出のため出力しません)")
			}
			if len(warnings) != 1 || warnings[0].Path != "github.app.privateKeyFile" {
				t.Fatalf("permission %04o の warning が不正です (warning 本文は出力しません)", tt.perm)
			}
			if strings.Contains(warnings[0].Message, keyPEM) {
				t.Fatal("warning に秘密の本文が含まれています")
			}
		})
	}
}

// TestLoad_SecretFileRejections covers unsafe and invalid secret files.
func TestLoad_SecretFileRejections(t *testing.T) {
	dir := t.TempDir()

	real := filepath.Join(dir, "real.pem")
	if err := os.WriteFile(real, []byte(keyPEM), 0600); err != nil {
		t.Fatalf("secret file の作成に失敗しました: %v", err)
	}
	link := filepath.Join(dir, "link.pem")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink の作成に失敗しました: %v", err)
	}
	dirPath := filepath.Join(dir, "subdir")
	if err := os.Mkdir(dirPath, 0700); err != nil {
		t.Fatalf("directory の作成に失敗しました: %v", err)
	}
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatalf("secret file の作成に失敗しました: %v", err)
	}
	big := filepath.Join(dir, "big.pem")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), 1<<20+1), 0600); err != nil {
		t.Fatalf("secret file の作成に失敗しました: %v", err)
	}

	tests := []struct {
		name    string
		keyPath string
		wantErr string
	}{
		{name: "symlink is rejected", keyPath: link, wantErr: "open secret file"},
		{name: "non regular file is rejected", keyPath: dirPath, wantErr: "secret file is not a regular file"},
		{name: "empty secret file is rejected", keyPath: empty, wantErr: "secret file is empty"},
		{name: "oversized secret file is rejected", keyPath: big, wantErr: "secret file is too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := strings.ReplaceAll(baseConfigYAML, "__KEY__", tt.keyPath)
			_, _, err := Load(writeConfig(t, doc))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatal("secret file の拒否が期待どおり動作しませんでした (error 本文は秘密露出のため出力しません)")
			}
			if strings.Contains(err.Error(), keyPEM) {
				t.Fatal("error に秘密の本文が含まれています")
			}
		})
	}
}

// TestReadSecretLimited_LimitAndGrowthDetection covers the read boundary.
func TestReadSecretLimited_LimitAndGrowthDetection(t *testing.T) {
	content, err := readSecretLimited(strings.NewReader(strings.Repeat("x", secretFileMaxSize)))
	if err != nil {
		t.Fatal("上限ちょうどの secret が拒否されました (error 本文は秘密露出のため出力しません)")
	}
	if len(content) != secretFileMaxSize {
		t.Fatalf("読み取り量が不正です: 期待値 %d、実測値 %d", secretFileMaxSize, len(content))
	}

	const marker = "SECRETPAYLOAD"
	_, err = readSecretLimited(strings.NewReader(strings.Repeat(marker, (secretFileMaxSize+1)/len(marker)+1)))
	if err == nil || !strings.Contains(err.Error(), "secret file is too large") {
		t.Fatal("上限超過の secret が拒否されませんでした (error 本文は秘密露出のため出力しません)")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatal("error に秘密の本文が含まれています")
	}

	content, err = readSecretLimited(strings.NewReader("small"))
	if err != nil || string(content) != "small" {
		t.Fatal("小さい secret の読み取りが不正です (内容は秘密のため出力しません)")
	}
}

// TestLoad_SecretNotLeakedOnFailure covers validation-time secret redaction.
func TestLoad_SecretNotLeakedOnFailure(t *testing.T) {
	token := "ghp_S3cr3t-pat-value-never-leak"
	t.Setenv("GITHUB_TOKEN", token)
	bad := strings.Replace(patRepoYAML, "  image: ubuntu\n", "  image: ubuntu:bad!\n", 1)
	_, _, err := Load(writeConfig(t, bad))
	if err == nil || !strings.Contains(err.Error(), "runner.image") {
		t.Fatal("validation failure が期待どおり発生しませんでした (error 本文は秘密露出のため出力しません)")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("error に PAT の本文が含まれています")
	}

	// Clear the PAT so it cannot conflict with the App.
	t.Setenv("GITHUB_TOKEN", "")
	keyPath := mustWriteSecret(t, "key.pem", keyPEM, 0600)
	badDoc := strings.Replace(baseConfigYAML, "__KEY__", keyPath, 1)
	badDoc = strings.Replace(badDoc, "  name: prod\n", "  name: \"bad name!\"\n", 1)
	_, _, err = Load(writeConfig(t, badDoc))
	if err == nil || !strings.Contains(err.Error(), "scaleSet.name") {
		t.Fatal("validation failure が期待どおり発生しませんでした (error 本文は秘密露出のため出力しません)")
	}
	if strings.Contains(err.Error(), keyPEM) {
		t.Fatal("error に private key の本文が含まれています")
	}
}
