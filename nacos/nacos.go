// Package nacos 包装 nacos-sdk-go/v2:配置中心 + 命名服务一次建好。
// register/get_config 只在启动、停机与热更路径调用(与 Rust 版一致);
// 配置不存在返回「未找到」而非错误;密钥不落日志。
package nacos

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nacos-group/nacos-sdk-go/v2/clients"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/config_client"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/naming_client"
	"github.com/nacos-group/nacos-sdk-go/v2/common/constant"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
	"github.com/pelletier/go-toml/v2"

	"hb"
)

// Config 环境变量语义的连接参数(DataID/Group 仅 FromEnv 填充)。
type Config struct {
	ServerAddr string // NACOS_SERVER_ADDR(必填)
	Namespace  string // NACOS_NAMESPACE,缺省 public
	Username   string // NACOS_USERNAME,与 Password 成对才启用鉴权
	Password   string // NACOS_PASSWORD
	DataID     string // NACOS_DATA_ID,缺省二进制名
	Group      string // NACOS_GROUP,缺省 DEFAULT_GROUP
}

// Nacos 已连接的配置中心 + 命名服务客户端。
type Nacos struct {
	config config_client.IConfigClient
	naming naming_client.INamingClient
}

// FromEnv 从环境变量解析连接参数:NACOS_SERVER_ADDR 必填、NACOS_NAMESPACE
// 缺省 public、NACOS_USERNAME/PASSWORD 成对才启用鉴权、NACOS_DATA_ID 缺省
// 二进制名、NACOS_GROUP 缺省 DEFAULT_GROUP。
func FromEnv() (Config, error) {
	serverAddr := os.Getenv("NACOS_SERVER_ADDR")
	if serverAddr == "" {
		return Config{}, hb.Err(500, "missing env: NACOS_SERVER_ADDR")
	}
	cfg := Config{
		ServerAddr: serverAddr,
		Namespace:  os.Getenv("NACOS_NAMESPACE"),
		Username:   os.Getenv("NACOS_USERNAME"),
		Password:   os.Getenv("NACOS_PASSWORD"),
		DataID:     os.Getenv("NACOS_DATA_ID"),
		Group:      os.Getenv("NACOS_GROUP"),
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "public"
	}
	if cfg.DataID == "" {
		cfg.DataID = filepath.Base(os.Args[0])
	}
	if cfg.Group == "" {
		cfg.Group = "DEFAULT_GROUP"
	}
	return cfg, nil
}

// Connect 连接 Nacos(无鉴权)。
func Connect(serverAddr, namespace string) (*Nacos, error) {
	return ConnectAuth(serverAddr, namespace, "", "")
}

// ConnectAuth 连接 Nacos;username 与 password 均非空才启用鉴权。
func ConnectAuth(serverAddr, namespace, username, password string) (*Nacos, error) {
	if serverAddr == "" {
		return nil, hb.Err(500, "nacos config: server addr is required")
	}
	var servers []constant.ServerConfig
	for _, addr := range strings.Split(serverAddr, ",") {
		host, portStr, err := splitHostPort(strings.TrimSpace(addr))
		if err != nil {
			return nil, hb.Errf(500, "nacos config: bad server addr %q: %v", addr, err)
		}
		servers = append(servers, constant.ServerConfig{
			IpAddr:      host,
			Port:        portStr,
			ContextPath: "/nacos",
			Scheme:      "http",
		})
	}
	clientCfg := &constant.ClientConfig{
		NamespaceId: namespace,
		TimeoutMs:   5000,
		LogLevel:    "warn",
	}
	if username != "" && password != "" {
		clientCfg.Username = username
		clientCfg.Password = password
	}
	param := &vo.NacosClientParam{ClientConfig: clientCfg, ServerConfigs: servers}
	configClient, err := clients.NewConfigClient(*param)
	if err != nil {
		return nil, hb.Internal(err)
	}
	namingClient, err := clients.NewNamingClient(*param)
	if err != nil {
		return nil, hb.Internal(err)
	}
	return &Nacos{config: configClient, naming: namingClient}, nil
}

// splitHostPort 拆 "host:port";端口省略时取 Nacos 默认 8848。
func splitHostPort(addr string) (string, uint64, error) {
	const defaultPort = 8848
	host, portStr, found := strings.Cut(addr, ":")
	if !found {
		return host, defaultPort, nil
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return "", 0, fmt.Errorf("bad port %q", portStr)
	}
	return host, port, nil
}

// GetConfig 读取配置文本;配置不存在返回 found=false。
func (n *Nacos) GetConfig(dataID, group string) (string, bool, error) {
	content, err := n.config.GetConfig(vo.ConfigParam{DataId: dataID, Group: group})
	if err != nil && strings.Contains(err.Error(), "not exist") {
		return "", false, nil
	}
	if err != nil {
		return "", false, hb.Internal(err)
	}
	if content == "" {
		return "", false, nil
	}
	return content, true, nil
}

// LoadConfig 读取并解析 TOML 配置到 target;配置不存在返回 found=false。
func (n *Nacos) LoadConfig(dataID, group string, target any) (bool, error) {
	text, found, err := n.GetConfig(dataID, group)
	if err != nil || !found {
		return false, err
	}
	if err := ParseConfig(text, target); err != nil {
		return true, err
	}
	return true, nil
}

// ParseConfig 解析 TOML 配置文本(与根包同款错误信封:config: <原因>)。
func ParseConfig(text string, target any) error {
	if err := toml.Unmarshal([]byte(text), target); err != nil {
		return hb.Errf(500, "config: %v", err)
	}
	return nil
}

// ListenConfig 监听配置变更,Nacos 推送时以完整新内容回调 onChange。
func (n *Nacos) ListenConfig(dataID, group string, onChange func(content string)) error {
	err := n.config.ListenConfig(vo.ConfigParam{
		DataId: dataID,
		Group:  group,
		OnChange: func(_, _, _, data string) {
			onChange(data)
		},
	})
	if err != nil {
		return hb.Internal(err)
	}
	return nil
}

// Register 注册临时服务实例(进程退出或超时后 Nacos 自动摘除)。
func (n *Nacos) Register(service, ip string, port uint64, group string) error {
	ok, err := n.naming.RegisterInstance(vo.RegisterInstanceParam{
		ServiceName: service,
		Ip:          ip,
		Port:        port,
		GroupName:   group,
		Weight:      1.0,
		Enable:      true,
		Healthy:     true,
		Ephemeral:   true,
	})
	if err != nil || !ok {
		return hb.Internal(err)
	}
	return nil
}

// Deregister 注销服务实例(boot 在停机时调用;临时实例漏调也会自动过期)。
func (n *Nacos) Deregister(service, ip string, port uint64, group string) error {
	ok, err := n.naming.DeregisterInstance(vo.DeregisterInstanceParam{
		ServiceName: service,
		Ip:          ip,
		Port:        port,
		GroupName:   group,
		Ephemeral:   true,
	})
	if err != nil {
		return hb.Internal(err)
	}
	_ = ok
	return nil
}
