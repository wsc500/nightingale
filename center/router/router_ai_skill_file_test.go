package router

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/aop"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupSkillFileAPI(t *testing.T) (*Router, *gin.Engine, *models.AISkill, *models.AISkillFile) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.AISkill{}, &models.AISkillFile{}, &models.UserGroupMember{}); err != nil {
		t.Fatal(err)
	}
	rt := &Router{Ctx: &ctx.Context{DB: db}}
	s := &models.AISkill{Name: "file-demo", Instructions: "demo", CreatedBy: "alice"}
	if err := db.Create(s).Error; err != nil {
		t.Fatal(err)
	}
	f := &models.AISkillFile{SkillId: s.Id, Name: "assets/图像.bin", Content: string([]byte{0, 1, 2, 255}), CreatedBy: "alice"}
	if err := f.Create(rt.Ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO ai_skill_file (skill_id, name, content, size) VALUES (?, ?, ?, ?)", s.Id, "legacy.txt", "old text", 8).Error; err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(aop.RecoveryWithWriter(io.Discard))
	pages := r.Group("/api/n9e", func(c *gin.Context) {
		c.Set("user", &models.User{Id: 7, Username: "bob"})
	})
	pages.GET("/ai-skill-file/:fileId", rt.aiSkillFileGet)
	pages.GET("/ai-skill-file/:fileId/download", rt.aiSkillFileDownload)
	r.GET("/v1/n9e/ai-skill/:id", rt.aiSkillGetWithFileContents)
	r.GET("/v1/n9e/ai-skill-file/:fileId/download", rt.aiSkillFileDownloadByService)
	return rt, r, s, f
}

func TestSkillFileDetailsReturnHashAndDownloadsReturnBytes(t *testing.T) {
	_, r, s, f := setupSkillFileAPI(t)
	fileID := strconv.FormatInt(f.Id, 10)
	for _, path := range []string{
		"/api/n9e/ai-skill-file/" + fileID,
		"/v1/n9e/ai-skill/" + strconv.FormatInt(s.Id, 10),
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", path, w.Code, w.Body)
		}
		var response struct {
			Data json.RawMessage `json:"dat"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(response.Data, []byte(`"content_hash":"`+f.ContentHash+`"`)) || bytes.Contains(response.Data, []byte("content_base64")) || bytes.Contains(response.Data, []byte("content_blob")) {
			t.Fatalf("GET %s leaked binary content or omitted the hash: %s", path, response.Data)
		}
		var binaryFile models.AISkillFile
		if strings.HasPrefix(path, "/v1/") {
			var detail models.AISkill
			if err := json.Unmarshal(response.Data, &detail); err != nil {
				t.Fatal(err)
			}
			for _, file := range detail.Files {
				if file.Id == f.Id {
					binaryFile = *file
				}
			}
			if !bytes.Contains(response.Data, []byte(`"content":"old text"`)) {
				t.Fatalf("service detail lost legacy text: %s", response.Data)
			}
		} else if err := json.Unmarshal(response.Data, &binaryFile); err != nil {
			t.Fatal(err)
		}
		if binaryFile.Content != "" || !binaryFile.IsBinary || binaryFile.ContentHash != f.ContentHash {
			t.Fatalf("binary response should contain metadata only: %+v", binaryFile)
		}
	}
	for _, prefix := range []string{"/api/n9e", "/v1/n9e"} {
		path := prefix + "/ai-skill-file/" + fileID + "/download"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), f.ContentBlob) {
			t.Fatalf("GET %s: status=%d bytes=%x", path, w.Code, w.Body.Bytes())
		}
		sum := sha256.Sum256(w.Body.Bytes())
		if hex.EncodeToString(sum[:]) != f.ContentHash || w.Header().Get("ETag") != `"`+f.ContentHash+`"` {
			t.Fatalf("download differs from its advertised hash: %s", path)
		}
		_, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
		if err != nil || params["filename"] != "图像.bin" {
			t.Fatalf("invalid download filename: params=%v err=%v", params, err)
		}
		if w.Header().Get("Content-Type") != "application/octet-stream" {
			t.Fatalf("unexpected download content type: %v", w.Header())
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Range", "bytes=1-2")
		w = httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), f.ContentBlob[1:3]) {
			t.Fatalf("range download failed: status=%d bytes=%x", w.Code, w.Body.Bytes())
		}
	}
}

func TestSkillFileLegacyTextDetailAndDownload(t *testing.T) {
	rt, r, s, _ := setupSkillFileAPI(t)
	f, err := models.AISkillFileGet(rt.Ctx, "skill_id = ? AND name = ?", s.Id, "legacy.txt")
	if err != nil || f == nil {
		t.Fatalf("get legacy file: file=%v err=%v", f, err)
	}
	path := "/api/n9e/ai-skill-file/" + strconv.FormatInt(f.Id, 10)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"content":"old text"`)) {
		t.Fatalf("legacy detail changed: status=%d body=%s", w.Code, w.Body)
	}
	for _, prefix := range []string{"/api/n9e", "/v1/n9e"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, prefix+"/ai-skill-file/"+strconv.FormatInt(f.Id, 10)+"/download", nil))
		if w.Code != http.StatusOK || w.Body.String() != "old text" {
			t.Fatalf("legacy download changed: status=%d body=%s", w.Code, w.Body)
		}
	}
}

func TestSkillFileDownloadEnforcesVisibilityAndMissingParent(t *testing.T) {
	rt, r, s, f := setupSkillFileAPI(t)
	if err := rt.Ctx.DB.Model(s).Update("private", 1).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/n9e/ai-skill-file/" + strconv.FormatInt(f.Id, 10)
	for _, suffix := range []string{"", "/download"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path+suffix, nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("private file should reject unauthorized read: status=%d body=%s", w.Code, w.Body)
		}
	}
	if err := rt.Ctx.DB.Create(&models.UserGroupMember{UserId: 7, GroupId: 9}).Error; err != nil {
		t.Fatal(err)
	}
	s.UserGroupIds = []int64{9}
	if err := rt.Ctx.DB.Model(s).Select("user_group_ids").Updates(s).Error; err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path+"/download", nil))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), f.ContentBlob) {
		t.Fatalf("authorized team cannot download: status=%d body=%s", w.Code, w.Body)
	}
	if err := rt.Ctx.DB.Delete(s).Error; err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"/api/n9e", "/v1/n9e"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, prefix+"/ai-skill-file/"+strconv.FormatInt(f.Id, 10)+"/download", nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("orphan file should return 404: status=%d body=%s", w.Code, w.Body)
		}
	}
}

func TestSkillArchiveUploadAboveOldLimits(t *testing.T) {
	// An uncompressed ZIP exercises both the old 10 MiB archive limit and
	// the old 16 MiB binary-file limit without a 500 MiB allocation.
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	md, err := zw.Create("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(md, "---\nname: large-asset\n---\nUse this skill."); err != nil {
		t.Fatal(err)
	}
	asset, err := zw.CreateHeader(&zip.FileHeader{Name: "asset.bin", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	content := make([]byte, 16*1024*1024+1)
	if _, err := asset.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("file", "large.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/n9e/ai-skills/import", &body)
	c.Request.Header.Set("Content-Type", form.FormDataContentType())
	_, _, files := extractSkillArchive(c)
	if c.Request.MultipartForm != nil {
		defer c.Request.MultipartForm.RemoveAll()
	}
	if len(files["asset.bin"]) != len(content) {
		t.Fatalf("large uploaded binary changed: got %d want %d", len(files["asset.bin"]), len(content))
	}
}
