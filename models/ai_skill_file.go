package models

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"gorm.io/gorm"
)

// 所有列都显式声明类型，与 docker/migratesql/migrate.sql 保持一致，理由同 AILLMConfig。
type AISkillFile struct {
	Id          int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	SkillId     int64  `json:"skill_id" gorm:"type:bigint;not null;default:0;index:idx_skill_id"`
	Name        string `json:"name" gorm:"type:varchar(255);not null;default:''"`
	Content     string `json:"content" gorm:"type:mediumtext;->"`
	ContentBlob []byte `json:"-" gorm:"type:longblob"`
	IsBinary    bool   `json:"is_binary" gorm:"type:boolean;not null;default:false"`
	ContentHash string `json:"content_hash,omitempty" gorm:"type:varchar(64);not null;default:''"`
	Size        int64  `json:"size" gorm:"type:bigint;not null;default:0"`
	CreatedAt   int64  `json:"created_at" gorm:"type:bigint;not null;default:0"`
	CreatedBy   string `json:"created_by" gorm:"type:varchar(64);not null;default:''"`
	UpdatedAt   int64  `json:"updated_at" gorm:"type:bigint;not null;default:0"`
	UpdatedBy   string `json:"updated_by" gorm:"type:varchar(64);not null;default:''"`
}

func (f *AISkillFile) TableName() string {
	return "ai_skill_file"
}

// MaxFilesPerSkill caps how many files (ai_skill_file rows, SKILL.md included)
// a single skill may hold. It is the single source of truth for this limit,
// shared with the archive extractor in aiagent/skill. The default of 1000 is
// overwritten at startup from cconf.AIAgent.MaxFilesPerSkill (see router init),
// and kept here as a safe fallback for code paths that run before injection.
var MaxFilesPerSkill = 1000

const (
	MaxSkillBinaryFileSize = 500 * 1024 * 1024
	MaxSkillTextFileSize   = 16 * 1024 * 1024
	MaxSkillMDSize         = 64 * 1024
)

// IsBinarySkillContent identifies bytes that cannot be returned as plain UTF-8 text.
func IsBinarySkillContent(content string) bool {
	return !utf8.ValidString(content) || strings.IndexByte(content, 0) >= 0
}

// ValidateAISkillFileSize is shared by import, database writes, and disk sync.
func ValidateAISkillFileSize(name string, size int64, isBinary bool) error {
	if name == "SKILL.md" {
		if size > MaxSkillMDSize {
			return fmt.Errorf("SKILL.md exceeds %dKB limit (%d bytes)", MaxSkillMDSize/1024, size)
		}
		return nil
	}
	limit := int64(MaxSkillTextFileSize)
	if isBinary {
		limit = MaxSkillBinaryFileSize
	}
	if size > limit {
		return fmt.Errorf("file %s exceeds %dMB limit (%d bytes)", name, limit/1024/1024, size)
	}
	return nil
}

// PostgresAISkillFile is the PostgreSQL-compatible variant of AISkillFile.
// PostgreSQL does not support mediumtext; its text type is unlimited.
type PostgresAISkillFile struct {
	Id          int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	SkillId     int64  `json:"skill_id" gorm:"type:bigint;not null;default:0;index:idx_skill_id"`
	Name        string `json:"name" gorm:"type:varchar(255);not null;default:''"`
	Content     string `json:"content" gorm:"type:text;->"`
	ContentBlob []byte `json:"-" gorm:"type:bytea"`
	IsBinary    bool   `json:"is_binary" gorm:"type:boolean;not null;default:false"`
	ContentHash string `json:"content_hash,omitempty" gorm:"type:varchar(64);not null;default:''"`
	Size        int64  `json:"size" gorm:"type:bigint;not null;default:0"`
	CreatedAt   int64  `json:"created_at" gorm:"type:bigint;not null;default:0"`
	CreatedBy   string `json:"created_by" gorm:"type:varchar(64);not null;default:''"`
	UpdatedAt   int64  `json:"updated_at" gorm:"type:bigint;not null;default:0"`
	UpdatedBy   string `json:"updated_by" gorm:"type:varchar(64);not null;default:''"`
}

func (f *PostgresAISkillFile) TableName() string {
	return "ai_skill_file"
}

// RawContent returns the original file bytes as a Go string. All new writes use
// ContentBlob; legacy rows that only have Content still read unchanged.
func (f *AISkillFile) RawContent() string {
	if f.ContentBlob != nil {
		return string(f.ContentBlob)
	}
	return f.Content
}

// prepareForWrite moves incoming content to the blob column. IsBinary describes
// whether the response contains a hash in place of the bytes.
func (f *AISkillFile) prepareForWrite() error {
	size := int64(len(f.Content))
	f.IsBinary = IsBinarySkillContent(f.Content)
	if f.ContentBlob != nil {
		size = int64(len(f.ContentBlob))
		f.IsBinary = !utf8.Valid(f.ContentBlob) || bytes.IndexByte(f.ContentBlob, 0) >= 0
	}
	if err := ValidateAISkillFileSize(f.Name, size, f.IsBinary); err != nil {
		return err
	}
	if f.ContentBlob == nil {
		f.ContentBlob = []byte(f.Content)
	}
	f.ContentHash = ""
	if f.IsBinary {
		sum := sha256.Sum256(f.ContentBlob)
		f.ContentHash = hex.EncodeToString(sum[:])
	}
	f.Content = ""
	return nil
}

// PrepareContentResponse preserves text responses and returns the stored SHA-256
// hash for binary files, computed alongside their content when writing.
func (f *AISkillFile) PrepareContentResponse() {
	if f.IsBinary {
		f.Content = ""
		return
	}
	f.ContentHash = ""
	f.Content = f.RawContent()
}

const aiSkillFileMetadataColumns = "id, skill_id, name, is_binary, content_hash, size, created_at, created_by, updated_at, updated_by"

// Binary content and its hash are written together. JSON responses only fetch
// binary metadata; text content still supports the legacy content column.
const aiSkillFileResponseColumns = aiSkillFileMetadataColumns + ", CASE WHEN is_binary THEN NULL ELSE content END AS content, CASE WHEN is_binary THEN NULL ELSE content_blob END AS content_blob"

func AISkillFileGets(c *ctx.Context, skillId int64) ([]*AISkillFile, error) {
	var lst []*AISkillFile
	err := DB(c).Select(aiSkillFileMetadataColumns).Where("skill_id = ?", skillId).Order("id").Find(&lst).Error
	return lst, err
}

func AISkillFileGet(c *ctx.Context, where string, args ...interface{}) (*AISkillFile, error) {
	var obj AISkillFile
	err := DB(c).Where(where, args...).First(&obj).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &obj, nil
}

func AISkillFileGetById(c *ctx.Context, id int64) (*AISkillFile, error) {
	return AISkillFileGet(c, "id = ?", id)
}

func AISkillFileGetMetadataById(c *ctx.Context, id int64) (*AISkillFile, error) {
	return aiSkillFileGetSelectedById(c, id, aiSkillFileMetadataColumns)
}

func AISkillFileGetResponseById(c *ctx.Context, id int64) (*AISkillFile, error) {
	return aiSkillFileGetSelectedById(c, id, aiSkillFileResponseColumns)
}

func aiSkillFileGetSelectedById(c *ctx.Context, id int64, columns string) (*AISkillFile, error) {
	var obj AISkillFile
	err := DB(c).Select(columns).Where("id = ?", id).First(&obj).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &obj, err
}

func AISkillFileGetResponses(c *ctx.Context, skillId int64) ([]*AISkillFile, error) {
	var files []*AISkillFile
	err := DB(c).Select(aiSkillFileResponseColumns).Where("skill_id = ?", skillId).Order("id").Find(&files).Error
	return files, err
}

func (f *AISkillFile) Create(c *ctx.Context) error {
	now := time.Now().Unix()
	if err := f.prepareForWrite(); err != nil {
		return err
	}
	f.Size = int64(len(f.ContentBlob))
	f.CreatedAt = now
	f.UpdatedAt = now
	f.UpdatedBy = f.CreatedBy

	var count int64
	DB(c).Model(&AISkillFile{}).Where("skill_id = ?", f.SkillId).Count(&count)
	if count >= int64(MaxFilesPerSkill) {
		return fmt.Errorf("max %d files per skill", MaxFilesPerSkill)
	}

	return Insert(c, f)
}

// BatchUpsert batch-upserts files for a given skill within a single transaction.
// When fullSync is true, existing files not present in the incoming list are deleted (full replace).
func AISkillFileBatchUpsert(c *ctx.Context, skillId int64, files []*AISkillFile, fullSync bool) error {
	return DB(c).Transaction(func(tx *gorm.DB) error {
		var existingFiles []*AISkillFile
		if err := tx.Select("id, name").Where("skill_id = ?", skillId).Find(&existingFiles).Error; err != nil {
			return err
		}

		existingMap := make(map[string]int64, len(existingFiles))
		for _, ef := range existingFiles {
			existingMap[ef.Name] = ef.Id
		}

		incomingNames := make(map[string]struct{}, len(files))
		for _, f := range files {
			incomingNames[f.Name] = struct{}{}
		}

		if fullSync {
			var staleIds []int64
			for _, ef := range existingFiles {
				if _, ok := incomingNames[ef.Name]; !ok {
					staleIds = append(staleIds, ef.Id)
				}
			}
			if len(staleIds) > 0 {
				if err := tx.Where("id IN ?", staleIds).Delete(&AISkillFile{}).Error; err != nil {
					return err
				}
			}
		}

		now := time.Now().Unix()
		var toInsert []*AISkillFile

		for _, f := range files {
			f.SkillId = skillId
			if err := f.prepareForWrite(); err != nil {
				return err
			}
			f.Size = int64(len(f.ContentBlob))
			f.CreatedAt = now
			f.UpdatedAt = now
			f.UpdatedBy = f.CreatedBy

			if existId, ok := existingMap[f.Name]; ok {
				if err := tx.Model(&AISkillFile{Id: existId}).Updates(map[string]interface{}{
					"content_blob": f.ContentBlob,
					"is_binary":    f.IsBinary,
					"content_hash": f.ContentHash,
					"size":         f.Size,
					"updated_at":   now,
					"updated_by":   f.CreatedBy,
				}).Error; err != nil {
					return err
				}
			} else {
				toInsert = append(toInsert, f)
			}
		}

		if len(toInsert) == 0 {
			return nil
		}

		// Re-count inside the transaction to narrow the TOCTOU window vs. concurrent writers.
		var totalCount int64
		if err := tx.Model(&AISkillFile{}).Where("skill_id = ?", skillId).Count(&totalCount).Error; err != nil {
			return err
		}
		if totalCount+int64(len(toInsert)) > int64(MaxFilesPerSkill) {
			return fmt.Errorf("max %d files per skill, current: %d, importing: %d", MaxFilesPerSkill, totalCount, len(toInsert))
		}

		// One file per statement keeps several large binaries from exceeding
		// the database packet limit in a single INSERT.
		return tx.CreateInBatches(&toInsert, 1).Error
	})
}

func (f *AISkillFile) Delete(c *ctx.Context) error {
	return DB(c).Where("id = ?", f.Id).Delete(&AISkillFile{}).Error
}

func AISkillFileDeleteBySkillId(c *ctx.Context, skillId int64) error {
	return DB(c).Where("skill_id = ?", skillId).Delete(&AISkillFile{}).Error
}

func AISkillFileGetContents(c *ctx.Context, skillId int64) ([]*AISkillFile, error) {
	var lst []*AISkillFile
	err := DB(c).Where("skill_id = ?", skillId).Find(&lst).Error
	return lst, err
}

// AISkillFilesBySkillIds returns all files for the given skill ids in a single
// query, grouped by skill id. Used by the startup skill sync to avoid the N+1
// round-trip that the single-skill helper would produce.
//
// Empty input returns an empty map (not nil) so callers can key into it without
// a nil-check.
func AISkillFilesBySkillIds(c *ctx.Context, ids []int64) (map[int64][]*AISkillFile, error) {
	out := make(map[int64][]*AISkillFile, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var lst []*AISkillFile
	if err := DB(c).Where("skill_id IN ?", ids).Order("skill_id, id").Find(&lst).Error; err != nil {
		return nil, err
	}
	for _, f := range lst {
		out[f.SkillId] = append(out[f.SkillId], f)
	}
	return out, nil
}
