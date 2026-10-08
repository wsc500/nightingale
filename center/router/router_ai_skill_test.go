package router

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ccfos/nightingale/v6/aiagent/skill"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSkillFilesBinaryAndLegacyRoundTrip(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.AISkillFile{}); err != nil {
		t.Fatal(err)
	}
	c := &ctx.Context{DB: db}
	const skillID = 42
	if err := db.Exec(`INSERT INTO ai_skill_file (skill_id, name, content, size)
		VALUES (?, ?, ?, ?), (?, ?, ?, ?)`, skillID, "SKILL.md", "---\nname: demo\ndescription: demo\n---\nUse this skill.", 50,
		skillID, "legacy.txt", "existing text", 13).Error; err != nil {
		t.Fatal(err)
	}

	binary := []byte{0x89, 'P', 'N', 'G', 0x00, 0xff}
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for name, content := range map[string][]byte{
		"demo/SKILL.md":         []byte("---\nname: demo\ndescription: demo\n---\nUse this skill."),
		"demo/assets/image.png": binary,
		"demo/notes.txt":        []byte("new text"),
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	extracted := t.TempDir()
	if err := skill.ExtractZip(archive.Bytes(), extracted); err != nil {
		t.Fatal(err)
	}
	imported, err := skill.Walk(extracted)
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertSkillFiles(c, skillID, imported, "bob", false); err != nil {
		t.Fatal(err)
	}
	files, err := models.AISkillFileGetContents(c, skillID)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]*models.AISkillFile, len(files))
	dbFiles := make([]skill.DBSkillFile, 0, len(files))
	for _, f := range files {
		byName[f.Name] = f
		dbFiles = append(dbFiles, skill.DBSkillFile{Name: f.Name, Content: f.RawContent()})
	}
	if f := byName["legacy.txt"]; f.IsBinary || f.RawContent() != "existing text" {
		t.Fatalf("legacy text changed: %+v", f)
	}
	if f := byName["notes.txt"]; f.IsBinary || f.RawContent() != "new text" || f.Content != "" || !bytes.Equal(f.ContentBlob, []byte("new text")) {
		t.Fatalf("new text changed: %+v", f)
	} else {
		f.PrepareContentResponse()
		if f.Content != "new text" || f.ContentHash != "" {
			t.Fatalf("text response changed: %+v", f)
		}
	}
	var textColumnIsNull bool
	if err := db.Raw(`SELECT content IS NULL FROM ai_skill_file WHERE skill_id = ? AND name = ?`, skillID, "notes.txt").Scan(&textColumnIsNull).Error; err != nil {
		t.Fatal(err)
	}
	if !textColumnIsNull {
		t.Fatal("new text was also written to the legacy content column")
	}
	asset := byName["assets/image.png"]
	if asset == nil || !asset.IsBinary || asset.Content != "" || !bytes.Equal(asset.ContentBlob, binary) || asset.Size != int64(len(binary)) {
		t.Fatalf("binary file was not stored as raw bytes: %+v", asset)
	}
	asset.PrepareContentResponse()
	sum := sha256.Sum256(binary)
	wantHash := hex.EncodeToString(sum[:])
	if asset.ContentHash != wantHash {
		t.Fatalf("binary response has wrong hash: %q", asset.ContentHash)
	}
	encoded, err := json.Marshal(asset)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"content_hash":"`+wantHash+`"`)) || bytes.Contains(encoded, []byte("content_blob")) || bytes.Contains(encoded, []byte("content_base64")) {
		t.Fatalf("binary JSON response should contain only its hash: %s", encoded)
	}
	metadata, err := models.AISkillFileGets(c, skillID)
	if err != nil {
		t.Fatal(err)
	}
	var binaryListed bool
	for _, f := range metadata {
		if f.Name == "assets/image.png" {
			binaryListed = f.IsBinary && f.ContentHash == wantHash
		}
	}
	if !binaryListed {
		t.Fatal("file listing did not identify the binary resource and hash")
	}
	response, err := models.AISkillFileGetResponseById(c, asset.Id)
	if err != nil {
		t.Fatal(err)
	}
	if response.ContentBlob != nil || response.Content != "" || response.ContentHash != wantHash {
		t.Fatalf("binary detail loaded the blob instead of only its hash: %+v", response)
	}

	root := t.TempDir()
	if err := skill.SyncOneDBSkill(root, skill.DBSkill{Name: "demo", Files: dbFiles}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "demo", "assets", "image.png"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("materialized binary differs: %x != %x", got, binary)
	}
}

func TestSkillFileUpdateKeepsBlobAndChangesBinaryFlag(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.AISkillFile{}); err != nil {
		t.Fatal(err)
	}
	c := &ctx.Context{DB: db}
	const skillID = 43
	for _, content := range []string{string([]byte{0xff, 0x00}), "plain text", string([]byte{'A', 0, 'B'}), string([]byte{0xfe, 0x01})} {
		if err := upsertSkillFiles(c, skillID, map[string]string{"asset.dat": content}, "bob", true); err != nil {
			t.Fatal(err)
		}
		files, err := models.AISkillFileGetContents(c, skillID)
		if err != nil || len(files) != 1 {
			t.Fatalf("read updated file: files=%+v err=%v", files, err)
		}
		f := files[0]
		if f.RawContent() != content || f.Size != int64(len(content)) {
			t.Fatalf("file content changed on update: %+v", f)
		}
		if f.Content != "" || !bytes.Equal(f.ContentBlob, []byte(content)) || f.IsBinary != (content != "plain text") {
			t.Fatalf("new content was not stored solely in blob: %+v", f)
		}
		wantHash := ""
		if f.IsBinary {
			sum := sha256.Sum256([]byte(content))
			wantHash = hex.EncodeToString(sum[:])
		}
		if f.ContentHash != wantHash {
			t.Fatalf("hash was not updated alongside content: got %q want %q", f.ContentHash, wantHash)
		}
	}
}

func TestSkillFileMigrationKeepsLegacyText(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE ai_skill_file (
		id integer PRIMARY KEY AUTOINCREMENT,
		skill_id bigint NOT NULL DEFAULT 0, name varchar(255) NOT NULL DEFAULT '',
		content mediumtext, size bigint NOT NULL DEFAULT 0,
		created_at bigint NOT NULL DEFAULT 0, created_by varchar(64) NOT NULL DEFAULT '',
		updated_at bigint NOT NULL DEFAULT 0, updated_by varchar(64) NOT NULL DEFAULT ''
	)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO ai_skill_file (skill_id, name, content, size) VALUES (1, 'legacy.txt', 'before migration', 16)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.AISkillFile{}); err != nil {
		t.Fatal(err)
	}
	files, err := models.AISkillFileGetContents(&ctx.Context{DB: db}, 1)
	if err != nil || len(files) != 1 {
		t.Fatalf("legacy file lost during migration: files=%+v err=%v", files, err)
	}
	if files[0].IsBinary || files[0].RawContent() != "before migration" {
		t.Fatalf("legacy content changed during migration: %+v", files[0])
	}
}

func TestDoSkillImportUpdateClearsCurrentCommitWhenArchiveImport(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.AISkill{}, &models.AISkillFile{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	c := &ctx.Context{DB: db}
	rt := &Router{Ctx: c}
	current := &models.AISkill{
		Name:         "git-skill",
		Description:  "old description",
		Instructions: "old instructions",
		Enabled:      true,
		SourceType:   models.AISkillSourceGit,
		GitInfo: &models.AISkillGitInfo{
			URL:           "https://github.com/example/skills.git",
			RefType:       skill.GitRefBranch,
			Ref:           "main",
			AuthType:      skill.GitAuthToken,
			Token:         "enc:token",
			Subdir:        "skills/demo",
			CurrentCommit: "abc123",
		},
		CreatedBy: "alice",
		UpdatedBy: "alice",
	}
	if err := current.Create(c); err != nil {
		t.Fatalf("create skill: %v", err)
	}
	if err := models.AISkillFileBatchUpsert(c, current.Id, []*models.AISkillFile{
		{Name: "stale.txt", Content: "stale", CreatedBy: "alice"},
	}, false); err != nil {
		t.Fatalf("seed files: %v", err)
	}

	files := map[string]string{
		"SKILL.md": "updated skill markdown",
		"new.txt":  "new content",
	}
	meta := skill.Frontmatter{Name: "git-skill", Description: "new description"}
	if err := rt.doSkillImportUpdate(current, meta, "new instructions", files, "bob", nil, nil); err != nil {
		t.Fatalf("import update: %v", err)
	}

	got, err := models.AISkillGetById(c, current.Id)
	if err != nil {
		t.Fatalf("get skill: %v", err)
	}
	if got.SourceType != models.AISkillSourceGit {
		t.Fatalf("source_type changed: %q", got.SourceType)
	}
	if got.GitInfo == nil {
		t.Fatalf("git_info was cleared")
	}
	if got.GitInfo.URL != current.GitInfo.URL ||
		got.GitInfo.RefType != current.GitInfo.RefType ||
		got.GitInfo.Ref != current.GitInfo.Ref ||
		got.GitInfo.AuthType != current.GitInfo.AuthType ||
		got.GitInfo.Token != current.GitInfo.Token ||
		got.GitInfo.Subdir != current.GitInfo.Subdir {
		t.Fatalf("git_info changed: got %+v want %+v", got.GitInfo, current.GitInfo)
	}
	if got.GitInfo.CurrentCommit != "" {
		t.Fatalf("current_commit was not cleared: %+v", got.GitInfo)
	}
	if got.Description != "new description" || got.Instructions != "new instructions" {
		t.Fatalf("skill content was not updated: %+v", got)
	}

	gotFiles, err := models.AISkillFileGetContents(c, current.Id)
	if err != nil {
		t.Fatalf("get files: %v", err)
	}
	byName := make(map[string]string, len(gotFiles))
	for _, f := range gotFiles {
		byName[f.Name] = f.RawContent()
	}
	if _, ok := byName["stale.txt"]; ok {
		t.Fatalf("stale file was not removed: %+v", byName)
	}
	if byName["SKILL.md"] != "updated skill markdown" || byName["new.txt"] != "new content" {
		t.Fatalf("files were not replaced: %+v", byName)
	}
}

// git skill 走 UpdateWithGit（其 Select 不含 user_group_ids/private），auth 必须由
// doSkillImportUpdate 在同一事务里单独持久化，否则「改公共为私有」会被静默丢弃。
func TestDoSkillImportUpdatePersistsAuthForGitSkill(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.AISkill{}, &models.AISkillFile{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	c := &ctx.Context{DB: db}
	rt := &Router{Ctx: c}

	current := &models.AISkill{
		Name:         "git-skill",
		Instructions: "old",
		Enabled:      true,
		SourceType:   models.AISkillSourceGit,
		GitInfo:      &models.AISkillGitInfo{URL: "https://x/y.git", RefType: skill.GitRefBranch, Ref: "main"},
		Private:      0, // 原本公共、无团队
		CreatedBy:    "alice",
		UpdatedBy:    "alice",
	}
	if err := current.Create(c); err != nil {
		t.Fatalf("create: %v", err)
	}

	auth := &skillAuthScope{Private: 1, UserGroupIds: []int64{7}}
	meta := skill.Frontmatter{Name: "git-skill"}
	if err := rt.doSkillImportUpdate(current, meta, "new", map[string]string{"SKILL.md": "md"}, "bob", current.GitInfo, auth); err != nil {
		t.Fatalf("import update: %v", err)
	}
	got, err := models.AISkillGetById(c, current.Id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Private != 1 || len(got.UserGroupIds) != 1 || got.UserGroupIds[0] != 7 {
		t.Fatalf("git skill auth not persisted: private=%d teams=%v", got.Private, got.UserGroupIds)
	}
}
