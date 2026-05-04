package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const agentVersion = "1.0.0"

var cachedPublicIP string

type LocalConfig struct {
	BaseURL     string `json:"base_url"`
	NodeToken   string `json:"node_token"`
	ServiceName string `json:"service_name"`
	StateDir    string `json:"state_dir"`
}

type AgentConfig struct {
	ConfigSchemaVersion int         `json:"config_schema_version"`
	ConfigVersion       int         `json:"config_version"`
	GroupID             int         `json:"group_id"`
	GeneratedAt         string      `json:"generated_at"`
	NftTable            string      `json:"nft_table"`
	Rules               []AgentRule `json:"rules"`
}

type AgentRule struct {
	ID         int        `json:"id"`
	ListenPort int        `json:"listen_port"`
	Protocol   string     `json:"protocol"`
	Enabled    bool       `json:"enabled"`
	Upstreams  []Upstream `json:"upstreams"`
}

type Upstream struct {
	ID      int    `json:"id"`
	IP      string `json:"ip"`
	Port    int    `json:"port"`
	Enabled bool   `json:"enabled"`
}

type HeartbeatResponse struct {
	Accepted      bool        `json:"accepted"`
	ConfigVersion int         `json:"config_version"`
	NeedsConfig   bool        `json:"needs_config"`
	Task          *ChangeTask `json:"task"`
	Settings      struct {
		HeartbeatIntervalSeconds   int `json:"heartbeat_interval_seconds"`
		HealthCheckIntervalSeconds int `json:"health_check_interval_seconds"`
		HealthCheckTimeoutMS       int `json:"health_check_timeout_ms"`
	} `json:"settings"`
}

type ChangeTask struct {
	ID     int    `json:"id"`
	Status string `json:"status"`
}

type HeartbeatPayload struct {
	PublicIP      string                 `json:"public_ip"`
	AgentVersion  string                 `json:"agent_version"`
	ServiceName   string                 `json:"service_name"`
	ConfigVersion int                    `json:"config_version"`
	NftStatus     string                 `json:"nft_status"`
	LastError     *string                `json:"last_error"`
	Metrics       map[string]interface{} `json:"metrics"`
	RuleMetrics   []RuleMetric           `json:"rule_metrics,omitempty"`
	Health        []HealthResult         `json:"health"`
	TaskResults   []TaskResult           `json:"task_results,omitempty"`
}

type RuleMetric struct {
	RuleID      int    `json:"rule_id"`
	RxBytes     uint64 `json:"rx_bytes"`
	TxBytes     uint64 `json:"tx_bytes"`
	Connections int    `json:"connections"`
}

type HealthResult struct {
	UpstreamID int     `json:"upstream_id"`
	Status     string  `json:"status"`
	LatencyMS  int64   `json:"latency_ms,omitempty"`
	Error      *string `json:"error,omitempty"`
}

type TaskResult struct {
	TaskID int     `json:"task_id"`
	Phase  string  `json:"phase"`
	Status string  `json:"status"`
	Error  *string `json:"error,omitempty"`
}

func main() {
	configPath := flag.String("config", "/etc/tforward-agent/agent.json", "agent config path")
	once := flag.Bool("once", false, "run one heartbeat then exit")
	ticks := flag.Int("ticks", 0, "run this many heartbeats then exit; 0 means forever")
	dryRun := flag.Bool("dry-run", false, "do not run nft, only generate config")
	flag.Parse()

	cfg, err := readLocalConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败: %v\n", err)
		os.Exit(1)
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "tforward-agent"
	}
	if cfg.StateDir == "" {
		cfg.StateDir = "/var/lib/tforward-agent"
	}
	client := &http.Client{Timeout: 10 * time.Second}
	runner := AgentRunner{cfg: cfg, client: client, dryRun: *dryRun}
	remainingTicks := *ticks
	if *once && remainingTicks == 0 {
		remainingTicks = 1
	}
	for {
		if err := runner.tick(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "心跳失败: %v\n", err)
		}
		if remainingTicks > 0 {
			remainingTicks -= 1
		}
		if remainingTicks == 0 && (*once || *ticks > 0) {
			return
		}
		interval := runner.heartbeatInterval
		if interval <= 0 {
			interval = 5 * time.Second
		}
		time.Sleep(interval)
	}
}

type AgentRunner struct {
	cfg               LocalConfig
	client            *http.Client
	dryRun            bool
	configVersion     int
	lastNftStatus     string
	lastError         *string
	heartbeatInterval time.Duration
	pendingResults    []TaskResult
}

func (r *AgentRunner) tick(ctx context.Context) error {
	health, _ := r.healthCheck(ctx)
	payload := HeartbeatPayload{
		PublicIP:      localPublicIP(),
		AgentVersion:  agentVersion,
		ServiceName:   r.cfg.ServiceName,
		ConfigVersion: r.configVersion,
		NftStatus:     r.lastNftStatus,
		LastError:     r.lastError,
		Metrics:       collectMetrics(),
		RuleMetrics:   r.collectRuleMetrics(ctx),
		Health:        health,
		TaskResults:   r.pendingResults,
	}
	r.pendingResults = nil
	var response HeartbeatResponse
	if err := r.post(ctx, "/api/v1/agent/heartbeat", payload, &response); err != nil {
		if strings.Contains(err.Error(), "401") {
			return errors.New("node token 无效，停止拉取新配置但保留本地 nftables")
		}
		return err
	}
	if response.Settings.HeartbeatIntervalSeconds > 0 {
		r.heartbeatInterval = time.Duration(response.Settings.HeartbeatIntervalSeconds) * time.Second
	}
	if response.Task != nil {
		r.pendingResults = append(r.pendingResults, r.handleTask(ctx, *response.Task))
		return nil
	}
	if response.NeedsConfig {
		cfg, err := r.fetchConfig(ctx)
		if err != nil {
			return err
		}
		if cfg.ConfigSchemaVersion != 1 {
			errText := fmt.Sprintf("不支持的配置 schema: %d", cfg.ConfigSchemaVersion)
			r.lastError = &errText
			r.lastNftStatus = "failed"
			return errors.New(errText)
		}
		if err := r.applyConfig(ctx, cfg); err != nil {
			errText := err.Error()
			r.lastError = &errText
			r.lastNftStatus = "failed"
			return err
		}
		r.configVersion = cfg.ConfigVersion
		r.lastError = nil
		r.lastNftStatus = "synced"
	}
	return nil
}

func (r *AgentRunner) handleTask(ctx context.Context, task ChangeTask) TaskResult {
	switch task.Status {
	case "prechecking":
		cfg, err := r.fetchConfig(ctx)
		if err != nil {
			return failedTask(task.ID, "precheck", err)
		}
		if err := precheckConfig(cfg); err != nil {
			return failedTask(task.ID, "precheck", err)
		}
		return TaskResult{TaskID: task.ID, Phase: "precheck", Status: "success"}
	case "applying":
		cfg, err := r.fetchConfig(ctx)
		if err != nil {
			return failedTask(task.ID, "apply", err)
		}
		if err := r.applyConfig(ctx, cfg); err != nil {
			errText := err.Error()
			r.lastError = &errText
			r.lastNftStatus = "failed"
			return failedTask(task.ID, "apply", err)
		}
		r.configVersion = cfg.ConfigVersion
		r.lastNftStatus = "synced"
		r.lastError = nil
		return TaskResult{TaskID: task.ID, Phase: "apply", Status: "success"}
	default:
		return TaskResult{TaskID: task.ID, Phase: "precheck", Status: "success"}
	}
}

func failedTask(taskID int, phase string, err error) TaskResult {
	text := err.Error()
	return TaskResult{TaskID: taskID, Phase: phase, Status: "failed", Error: &text}
}

func readLocalConfig(path string) (LocalConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LocalConfig{}, err
	}
	var cfg LocalConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return LocalConfig{}, err
	}
	if cfg.BaseURL == "" || cfg.NodeToken == "" {
		return LocalConfig{}, errors.New("base_url 和 node_token 必填")
	}
	return cfg, nil
}

func (r *AgentRunner) fetchConfig(ctx context.Context) (AgentConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.cfg.BaseURL+"/api/v1/agent/config", nil)
	if err != nil {
		return AgentConfig{}, err
	}
	req.Header.Set("Authorization", "Bearer "+r.cfg.NodeToken)
	res, err := r.client.Do(req)
	if err != nil {
		return AgentConfig{}, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		body, _ := io.ReadAll(res.Body)
		return AgentConfig{}, fmt.Errorf("拉取配置失败 %d: %s", res.StatusCode, string(body))
	}
	var cfg AgentConfig
	return cfg, json.NewDecoder(res.Body).Decode(&cfg)
}

func (r *AgentRunner) post(ctx context.Context, path string, in interface{}, out interface{}) error {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.cfg.NodeToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		data, _ := io.ReadAll(res.Body)
		return fmt.Errorf("请求失败 %d: %s", res.StatusCode, string(data))
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func (r *AgentRunner) applyConfig(ctx context.Context, cfg AgentConfig) error {
	text := buildNftConfig(cfg)
	if err := os.MkdirAll(r.cfg.StateDir, 0755); err != nil {
		return err
	}
	path := filepath.Join(r.cfg.StateDir, "current.nft")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		return err
	}
	if r.dryRun {
		return nil
	}
	if err := runNft(ctx, "-c", "-f", path); err != nil {
		return fmt.Errorf("nft 语法校验失败: %w", err)
	}
	if err := runNft(ctx, "-f", path); err != nil {
		return fmt.Errorf("nft 应用失败: %w", err)
	}
	if err := validateAppliedNftTable(ctx, cfg); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.cfg.StateDir, "last-success.nft"), []byte(text), 0600); err != nil {
		return err
	}
	return nil
}

func validateAppliedNftTable(ctx context.Context, cfg AgentConfig) error {
	cmd := exec.CommandContext(ctx, "nft", "-j", "list", "table", "inet", "tforward")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("nft 应用后检查失败: %w", err)
	}
	var payload interface{}
	if err := json.Unmarshal(output, &payload); err != nil {
		return fmt.Errorf("nft 应用后检查返回了非法 JSON: %w", err)
	}
	found := map[string]int{}
	collectNftComments(payload, found)
	for _, comment := range expectedRuleComments(cfg) {
		if found[comment] == 0 {
			return fmt.Errorf("nft 应用后检查失败：缺少规则 comment %s", comment)
		}
	}
	return nil
}

func precheckConfig(cfg AgentConfig) error {
	if cfg.ConfigSchemaVersion != 1 {
		return fmt.Errorf("不支持的配置 schema: %d", cfg.ConfigSchemaVersion)
	}
	seenPorts := map[int]int{}
	for _, rule := range cfg.Rules {
		if rule.Protocol != "tcp" && rule.Protocol != "udp" && rule.Protocol != "both" {
			return fmt.Errorf("规则 #%d 协议不合法: %s", rule.ID, rule.Protocol)
		}
		if rule.ListenPort < 1 || rule.ListenPort > 65535 {
			return fmt.Errorf("规则 #%d 入口端口不合法: %d", rule.ID, rule.ListenPort)
		}
		if other, ok := seenPorts[rule.ListenPort]; ok {
			return fmt.Errorf("规则 #%d 入口端口 %d 已被规则 #%d 占用", rule.ID, rule.ListenPort, other)
		}
		seenPorts[rule.ListenPort] = rule.ID
		active := enabledUpstreams(rule.Upstreams)
		if rule.Enabled && len(active) == 0 {
			return fmt.Errorf("规则 #%d 启用时至少需要一个启用落地", rule.ID)
		}
		for _, upstream := range rule.Upstreams {
			ip := net.ParseIP(upstream.IP)
			if ip == nil || ip.To4() == nil {
				return fmt.Errorf("规则 #%d 落地 %s:%d 不是合法 IPv4", rule.ID, upstream.IP, upstream.Port)
			}
			if upstream.Port < 1 || upstream.Port > 65535 {
				return fmt.Errorf("规则 #%d 落地端口不合法: %d", rule.ID, upstream.Port)
			}
		}
		if rule.Enabled {
			if rule.Protocol == "tcp" || rule.Protocol == "both" {
				if err := checkTCPPort(rule.ListenPort); err != nil {
					return fmt.Errorf("规则 #%d 入口端口 %d TCP 被占用: %w", rule.ID, rule.ListenPort, err)
				}
			}
			if rule.Protocol == "udp" || rule.Protocol == "both" {
				if err := checkUDPPort(rule.ListenPort); err != nil {
					return fmt.Errorf("规则 #%d 入口端口 %d UDP 被占用: %w", rule.ID, rule.ListenPort, err)
				}
			}
		}
	}
	return nil
}

func checkTCPPort(port int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("%w%s", err, portOwnerHint("tcp", port))
	}
	return listener.Close()
}

func checkUDPPort(port int) error {
	conn, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("%w%s", err, portOwnerHint("udp", port))
	}
	return conn.Close()
}

func portOwnerHint(protocol string, port int) string {
	args := []string{"-H"}
	if protocol == "udp" {
		args = append(args, "-lunp")
	} else {
		args = append(args, "-ltnp")
	}
	args = append(args, "sport", "=", fmt.Sprintf(":%d", port))
	output, err := exec.Command("ss", args...).CombinedOutput()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.Split(string(output), "\n")[0])
	if line == "" {
		return ""
	}
	return "；可能占用进程: " + line
}

func runNft(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "nft", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, string(output))
	}
	return nil
}

func buildNftConfig(cfg AgentConfig) string {
	var lines []string
	lines = append(lines, "flush table inet tforward", "table inet tforward {", "  chain prerouting {", "    type nat hook prerouting priority dstnat; policy accept;")
	targets := map[string]bool{}
	for _, rule := range cfg.Rules {
		if !rule.Enabled {
			continue
		}
		active := enabledUpstreams(rule.Upstreams)
		if len(active) == 0 {
			continue
		}
		for _, upstream := range active {
			targets[upstream.IP] = true
		}
		if rule.Protocol == "tcp" || rule.Protocol == "both" {
			lines = append(lines, nftDnatLine(rule, active, "tcp"))
		}
		if rule.Protocol == "udp" || rule.Protocol == "both" {
			lines = append(lines, nftDnatLine(rule, active, "udp"))
		}
	}
	lines = append(lines, "  }", "  chain postrouting {", "    type nat hook postrouting priority srcnat; policy accept;")
	for ip := range targets {
		lines = append(lines, "    ip daddr "+ip+" counter masquerade")
	}
	lines = append(lines, "  }", "}")
	return strings.Join(lines, "\n")
}

func enabledUpstreams(items []Upstream) []Upstream {
	var out []Upstream
	for _, item := range items {
		if item.Enabled {
			out = append(out, item)
		}
	}
	return out
}

func nftDnatLine(rule AgentRule, upstreams []Upstream, proto string) string {
	parts := make([]string, 0, len(upstreams))
	for i, upstream := range upstreams {
		parts = append(parts, fmt.Sprintf("%d : %s . %d", i, upstream.IP, upstream.Port))
	}
	return fmt.Sprintf("    ip protocol %s %s dport %d counter dnat ip to numgen inc mod %d map { %s } comment \"tforward-rule-%d-%s\"", proto, proto, rule.ListenPort, len(upstreams), strings.Join(parts, ", "), rule.ID, proto)
}

func (r *AgentRunner) collectRuleMetrics(ctx context.Context) []RuleMetric {
	cfg, err := r.fetchConfig(ctx)
	if err != nil {
		return nil
	}
	metrics := map[int]*RuleMetric{}
	if !r.dryRun {
		for ruleID, bytesValue := range nftRuleBytes(ctx) {
			item := metrics[ruleID]
			if item == nil {
				item = &RuleMetric{RuleID: ruleID}
				metrics[ruleID] = item
			}
			item.RxBytes += bytesValue
			item.TxBytes += bytesValue
		}
	}
	connections := conntrackConnections(cfg)
	for _, rule := range cfg.Rules {
		item := metrics[rule.ID]
		if item == nil {
			item = &RuleMetric{RuleID: rule.ID}
			metrics[rule.ID] = item
		}
		item.Connections = connections[rule.ID]
	}
	out := make([]RuleMetric, 0, len(metrics))
	for _, item := range metrics {
		out = append(out, *item)
	}
	return out
}

func nftRuleBytes(ctx context.Context) map[int]uint64 {
	cmd := exec.CommandContext(ctx, "nft", "-j", "list", "table", "inet", "tforward")
	output, err := cmd.Output()
	if err != nil {
		return map[int]uint64{}
	}
	var payload interface{}
	if err := json.Unmarshal(output, &payload); err != nil {
		return map[int]uint64{}
	}
	out := map[int]uint64{}
	collectNftCounters(payload, out)
	return out
}

func collectNftCounters(value interface{}, out map[int]uint64) {
	switch typed := value.(type) {
	case []interface{}:
		for _, item := range typed {
			collectNftCounters(item, out)
		}
	case map[string]interface{}:
		if ruleValue, ok := typed["rule"]; ok {
			ruleID := findRuleComment(ruleValue)
			bytesValue := findCounterBytes(ruleValue)
			if ruleID > 0 && bytesValue > 0 {
				out[ruleID] += bytesValue
			}
			return
		}
		for _, item := range typed {
			collectNftCounters(item, out)
		}
	}
}

func collectNftComments(value interface{}, out map[string]int) {
	switch typed := value.(type) {
	case []interface{}:
		for _, item := range typed {
			collectNftComments(item, out)
		}
	case map[string]interface{}:
		if raw, ok := typed["comment"].(string); ok {
			out[raw] += 1
		}
		for _, item := range typed {
			collectNftComments(item, out)
		}
	}
}

func expectedRuleComments(cfg AgentConfig) []string {
	var comments []string
	for _, rule := range cfg.Rules {
		if !rule.Enabled || len(enabledUpstreams(rule.Upstreams)) == 0 {
			continue
		}
		if rule.Protocol == "tcp" || rule.Protocol == "both" {
			comments = append(comments, fmt.Sprintf("tforward-rule-%d-tcp", rule.ID))
		}
		if rule.Protocol == "udp" || rule.Protocol == "both" {
			comments = append(comments, fmt.Sprintf("tforward-rule-%d-udp", rule.ID))
		}
	}
	return comments
}

func findRuleComment(value interface{}) int {
	switch typed := value.(type) {
	case []interface{}:
		for _, item := range typed {
			if found := findRuleComment(item); found > 0 {
				return found
			}
		}
	case map[string]interface{}:
		if raw, ok := typed["comment"].(string); ok {
			return parseRuleComment(raw)
		}
		for _, item := range typed {
			if found := findRuleComment(item); found > 0 {
				return found
			}
		}
	}
	return 0
}

func parseRuleComment(comment string) int {
	if !strings.HasPrefix(comment, "tforward-rule-") {
		return 0
	}
	rest := strings.TrimPrefix(comment, "tforward-rule-")
	parts := strings.Split(rest, "-")
	if len(parts) == 0 {
		return 0
	}
	id, _ := strconv.Atoi(parts[0])
	return id
}

func findCounterBytes(value interface{}) uint64 {
	switch typed := value.(type) {
	case []interface{}:
		for _, item := range typed {
			if found := findCounterBytes(item); found > 0 {
				return found
			}
		}
	case map[string]interface{}:
		if counter, ok := typed["counter"].(map[string]interface{}); ok {
			if raw, ok := counter["bytes"].(float64); ok {
				return uint64(raw)
			}
		}
		for _, item := range typed {
			if found := findCounterBytes(item); found > 0 {
				return found
			}
		}
	}
	return 0
}

func conntrackConnections(cfg AgentConfig) map[int]int {
	data, err := os.ReadFile("/proc/net/nf_conntrack")
	if err != nil {
		data, err = os.ReadFile("/proc/net/ip_conntrack")
	}
	if err != nil {
		return map[int]int{}
	}
	out := map[int]int{}
	lines := strings.Split(string(data), "\n")
	for _, rule := range cfg.Rules {
		if !rule.Enabled {
			continue
		}
		needle := fmt.Sprintf("dport=%d", rule.ListenPort)
		for _, line := range lines {
			if strings.Contains(line, needle) {
				out[rule.ID] += 1
			}
		}
	}
	return out
}

func (r *AgentRunner) healthCheck(ctx context.Context) ([]HealthResult, error) {
	cfg, err := r.fetchConfig(ctx)
	if err != nil {
		return nil, err
	}
	timeout := 800 * time.Millisecond
	var results []HealthResult
	for _, rule := range cfg.Rules {
		if !rule.Enabled {
			continue
		}
		for _, upstream := range rule.Upstreams {
			if !upstream.Enabled {
				continue
			}
			start := time.Now()
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", upstream.IP, upstream.Port), timeout)
			if err != nil {
				msg := simplifyNetError(err)
				results = append(results, HealthResult{UpstreamID: upstream.ID, Status: "down", Error: &msg})
				continue
			}
			_ = conn.Close()
			results = append(results, HealthResult{UpstreamID: upstream.ID, Status: "up", LatencyMS: time.Since(start).Milliseconds()})
		}
	}
	return results, nil
}

func simplifyNetError(err error) string {
	text := err.Error()
	switch {
	case strings.Contains(text, "timeout"):
		return "连接超时"
	case strings.Contains(text, "refused"):
		return "连接被拒绝"
	case strings.Contains(text, "network is unreachable"):
		return "网络不可达"
	default:
		return "其他错误"
	}
}

func collectMetrics() map[string]interface{} {
	cpu := sampleCPUPercent()
	memory := memoryPercent()
	disk := diskPercent("/")
	uptime := uptimeSeconds()
	rx, tx := networkBytes()
	return map[string]interface{}{
		"cpu_percent":    cpu,
		"memory_percent": memory,
		"disk_percent":   disk,
		"uptime_seconds": uptime,
		"rx_bytes":       rx,
		"tx_bytes":       tx,
		"os":             runtime.GOOS,
	}
}

func sampleCPUPercent() float64 {
	idle1, total1, err := readCPUStat()
	if err != nil {
		return 0
	}
	time.Sleep(100 * time.Millisecond)
	idle2, total2, err := readCPUStat()
	if err != nil || total2 <= total1 {
		return 0
	}
	idleDelta := idle2 - idle1
	totalDelta := total2 - total1
	return roundPercent(100 * float64(totalDelta-idleDelta) / float64(totalDelta))
}

func readCPUStat() (uint64, uint64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	line := strings.SplitN(string(data), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, errors.New("无法读取 /proc/stat CPU 行")
	}
	var total uint64
	var idle uint64
	for index, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return 0, 0, err
		}
		total += value
		if index == 3 || index == 4 {
			idle += value
		}
	}
	return idle, total, nil
}

func memoryPercent() float64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	values := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		value, _ := strconv.ParseUint(fields[1], 10, 64)
		values[key] = value
	}
	total := values["MemTotal"]
	available := values["MemAvailable"]
	if total == 0 {
		return 0
	}
	return roundPercent(100 * float64(total-available) / float64(total))
}

func diskPercent(path string) float64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil || stat.Blocks == 0 {
		return 0
	}
	used := stat.Blocks - stat.Bavail
	return roundPercent(100 * float64(used) / float64(stat.Blocks))
}

func uptimeSeconds() uint64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	first := strings.Fields(string(data))
	if len(first) == 0 {
		return 0
	}
	value, _ := strconv.ParseFloat(first[0], 64)
	return uint64(value)
}

func networkBytes() (uint64, uint64) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	var rx uint64
	var tx uint64
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		name := strings.TrimSpace(parts[0])
		if name == "lo" {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 16 {
			continue
		}
		rxValue, _ := strconv.ParseUint(fields[0], 10, 64)
		txValue, _ := strconv.ParseUint(fields[8], 10, 64)
		rx += rxValue
		tx += txValue
	}
	return rx, tx
}

func roundPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	return float64(int(value*10+0.5)) / 10
}

func localPublicIP() string {
	if value := os.Getenv("TFORWARD_PUBLIC_IP"); value != "" {
		return value
	}
	if cachedPublicIP != "" {
		return cachedPublicIP
	}
	client := http.Client{Timeout: 2 * time.Second}
	res, err := client.Get("https://api.ipify.org")
	if err == nil {
		defer res.Body.Close()
		if res.StatusCode == 200 {
			data, _ := io.ReadAll(io.LimitReader(res.Body, 64))
			ip := strings.TrimSpace(string(data))
			if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
				cachedPublicIP = ip
				return cachedPublicIP
			}
		}
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if ip := ipNet.IP.To4(); ip != nil {
			return ip.String()
		}
	}
	return "127.0.0.1"
}
