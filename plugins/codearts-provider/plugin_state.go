package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Not .json: CPA must never load these files as account credentials.
const pluginStateDir = ".codearts-provider-state"

func pluginStatePath(authDirectory, name string) (string, error) {
	if !filepath.IsAbs(authDirectory) {
		return "", fmt.Errorf("无法确定认证目录，请先添加账号后重试")
	}
	dir := filepath.Join(authDirectory, pluginStateDir)
	return statePathInDirectory(dir, name)
}

func (c *Config) statePath(authDirectory, name string) (string, error) {
	if c.StateDir != "" {
		return statePathInDirectory(c.StateDir, name)
	}
	return pluginStatePath(authDirectory, name)
}

func statePathInDirectory(dir, name string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("state_dir 必须是绝对路径")
	}
	info, err := os.Lstat(dir)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("无法读取插件状态目录")
	}
	if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return "", fmt.Errorf("插件状态目录不是普通目录")
	}
	return filepath.Join(dir, name), nil
}

func readPluginState(path string, out any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return fmt.Errorf("插件状态文件类型或大小无效")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("无法读取插件状态文件")
	}
	if json.Unmarshal(b, out) != nil {
		return fmt.Errorf("插件状态文件损坏")
	}
	return nil
}

// Atomic replacement in the same directory. Never touch CPA's config or auth JSON.
func savePluginState(path string, state any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("无法创建插件状态目录")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("插件状态目录无效")
	}
	if info, err = os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("插件状态文件不是普通文件")
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("无法检查插件状态文件")
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".plugin-state-*.tmp")
	if err != nil {
		return fmt.Errorf("无法写入插件状态目录")
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return fmt.Errorf("保存插件状态失败")
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("替换插件状态文件失败")
	}
	return nil
}
