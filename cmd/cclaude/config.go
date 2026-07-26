package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// C14客户端寻址（console-fleet-design §6.7）：二进制不写死任何域名——
// 官网下载的产物对所有用户、所有节点通用。API地址是用户配置：
//
//	优先级：--api参数 > CCW_API环境变量 > ~/.ccw/config.json > 交互提示
//
// ~/.ccw/config.json（0600）取代旧的单行~/.ccw/cdk文件；启动时自动迁移旧文件。
// CDK永远不做命令行参数：参数会进shell history与ps输出（A24）。
type localConfig struct {
	API string `json:"api"`
	CDK string `json:"cdk"`
}

func configDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ccw")
}

func configPath(dir string) string { return filepath.Join(dir, "config.json") }
func legacyPath(dir string) string { return filepath.Join(dir, "cdk") }

// loadLocalConfig读本地配置；config.json不存在时尝试迁移旧的单行cdk文件（A23）。
//
// 损坏的config.json如实报错，不静默当成空配置——那会让下一次save把用户的CDK丢掉。
func loadLocalConfig(dir string) (localConfig, error) {
	b, err := os.ReadFile(configPath(dir))
	if err == nil {
		var c localConfig
		if jerr := json.Unmarshal(b, &c); jerr != nil {
			return localConfig{}, fmt.Errorf("解析%s失败（如需重置请删除该文件或执行 cclaude logout）: %w", configPath(dir), jerr)
		}
		return c, nil
	}
	if !os.IsNotExist(err) {
		return localConfig{}, err
	}
	// 旧格式迁移：仅在config.json不存在时进行；迁移成功后删除旧文件。
	if lb, lerr := os.ReadFile(legacyPath(dir)); lerr == nil {
		c := localConfig{CDK: strings.TrimSpace(string(lb))}
		if serr := saveLocalConfig(dir, c); serr != nil {
			return localConfig{}, serr
		}
		os.Remove(legacyPath(dir))
		return c, nil
	}
	return localConfig{}, nil
}

// saveLocalConfig写配置：目录0700、文件0600（A22）——CDK在文件里，权限即安全边界。
func saveLocalConfig(dir string, c localConfig) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(dir), append(b, '\n'), 0o600)
}

// clearLocalConfig是logout：删除新旧两种格式的本地配置，幂等。
func clearLocalConfig(dir string) error {
	for _, p := range []string{configPath(dir), legacyPath(dir)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// clearCDKField只清CDK、保留API：认证失败时用户重输CDK即可，不必重配域名。
func clearCDKField(dir string) error {
	c, err := loadLocalConfig(dir)
	if err != nil {
		// 配置本身读不出来（损坏等）：退回整体清除，让用户重新配置。
		return clearLocalConfig(dir)
	}
	if c.CDK == "" {
		return nil
	}
	c.CDK = ""
	return saveLocalConfig(dir, c)
}

// normalizeAPI校验并归一化API地址：必须是http/https绝对URL（http用于IP直连测试，
// DEPLOY.md第9节），去首尾空白与尾斜杠。
//
// 无路径时自动补/api：公开路径合同是`/api/* → control-api`（Caddyfile与spec §3），
// 因此用户只需给出域名（与设计§6.6的`/connect`页面示例一致），
// 而`https://域名/api`的完整写法同样接受（显式路径原样保留）。
func normalizeAPI(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("API地址为空")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("API地址无法解析: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("API地址必须以http://或https://开头（得到%q）", s)
	}
	if u.Host == "" {
		return "", fmt.Errorf("API地址缺少主机名: %q", s)
	}
	s = strings.TrimSuffix(s, "/")
	if u.Path == "" || u.Path == "/" {
		s += "/api"
	}
	return s, nil
}

// resolveAPI按优先级取API地址（§6.7）：
//
//	--api        显式换节点：回写本地配置
//	CCW_API      会话级覆盖：不回写
//	config.json  日常路径
//	交互提示      首次配置：先域名后CDK（域名输错立刻能从连接失败看出来）
func resolveAPI(dir, flagVal string, getenv func(string) string, prompt func(string) (string, error)) (string, error) {
	if flagVal != "" {
		api, err := normalizeAPI(flagVal)
		if err != nil {
			return "", err
		}
		c, lerr := loadLocalConfig(dir)
		if lerr != nil {
			c = localConfig{} // 配置损坏时以--api为准重建
		}
		c.API = api
		if err := saveLocalConfig(dir, c); err != nil {
			return "", err
		}
		return api, nil
	}
	if v := getenv("CCW_API"); v != "" {
		return normalizeAPI(v)
	}
	c, err := loadLocalConfig(dir)
	if err != nil {
		return "", err
	}
	if c.API != "" {
		return normalizeAPI(c.API)
	}
	in, err := prompt("API地址（如 https://api-01.example.com，可在 /connect 页面查询）: ")
	if err != nil {
		return "", err
	}
	api, err := normalizeAPI(in)
	if err != nil {
		return "", err
	}
	c.API = api
	if err := saveLocalConfig(dir, c); err != nil {
		return "", err
	}
	return api, nil
}

// resolveCDK按优先级取CDK：CCW_CDK环境变量 > config.json > 交互输入（term.ReadPassword）。
// 环境变量提供的CDK不写盘（会话级）；交互输入的持久化并保留API字段。
func resolveCDK(dir string, getenv func(string) string, promptSecret func(string) (string, error)) (string, error) {
	if v := getenv("CCW_CDK"); v != "" {
		return v, nil
	}
	c, err := loadLocalConfig(dir)
	if err != nil {
		return "", err
	}
	if c.CDK != "" {
		return c.CDK, nil
	}
	in, err := promptSecret("CDK: ")
	if err != nil {
		return "", err
	}
	cdk := strings.TrimSpace(in)
	if cdk == "" {
		return "", fmt.Errorf("未输入CDK")
	}
	c.CDK = cdk
	if err := saveLocalConfig(dir, c); err != nil {
		return "", err
	}
	return cdk, nil
}
