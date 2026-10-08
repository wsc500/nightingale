package skill

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ccfos/nightingale/v6/models"
)

// Walk 走读已解压的 skill 目录，返回 relPath → content 的文件映射。
// SKILL.md 和其它文件一视同仁放在 map 里，调用方自己从 files["SKILL.md"] 取主文件。
//
// 实现会自动 unwrap 单层顶级目录（archive root，见 archiveRoot），
// 并跳过 .* 和 __MACOSX 等系统噪声条目。SKILL.md 单独走 MaxSkillMD 限额，
// 文本附件和二进制附件分别走 16 MiB 与 500 MiB 限额。
func Walk(dir string) (files map[string]string, err error) {
	dir = archiveRoot(dir)
	files = make(map[string]string)
	var totalSize int64

	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		relPath = filepath.ToSlash(relPath)

		if relPath == "." {
			return nil
		}

		// 跳过隐藏文件 / macOS 归档元数据
		if strings.HasPrefix(filepath.Base(relPath), ".") || strings.HasPrefix(relPath, "__MACOSX") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink not allowed: %s", relPath)
		}

		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// Reject oversized files before allocating their content, including
		// Git files that did not pass through the archive extractor.
		if err := models.ValidateAISkillFileSize(relPath, info.Size(), true); err != nil {
			return err
		}
		if totalSize+info.Size() > MaxTotalExtracted {
			return fmt.Errorf("total extracted size exceeds %dMB limit", MaxTotalExtracted/1024/1024)
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		if err := models.ValidateAISkillFileSize(relPath, int64(len(content)), models.IsBinarySkillContent(string(content))); err != nil {
			return err
		}
		totalSize += int64(len(content))
		if totalSize > MaxTotalExtracted {
			return fmt.Errorf("total extracted size exceeds %dMB limit", MaxTotalExtracted/1024/1024)
		}
		files[relPath] = string(content)
		return nil
	})
	return
}

// archiveRoot 在归档中有单层顶级目录时返回那层目录，否则返回 dir。
// 这允许 zip 作者用 "my-skill/SKILL.md" 这种带 wrapper 的风格打包，
// 也允许裸 SKILL.md 直接放根目录。
func archiveRoot(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return dir
	}

	// 根目录下若有任何非隐藏文件，说明这就是 skill 根，不需要 unwrap
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			return dir
		}
	}

	var candidate string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "__MACOSX" {
			continue
		}
		if candidate != "" {
			// 多于一个真实顶层目录 —— 没有单层 wrapper
			return dir
		}
		candidate = name
	}

	if candidate != "" {
		return filepath.Join(dir, candidate)
	}
	return dir
}
