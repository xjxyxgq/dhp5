package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type configXMLNode struct {
	XMLName xml.Name
	Attrs   []xml.Attr      `xml:",any,attr"`
	Nodes   []configXMLNode `xml:",any"`
	Text    string          `xml:",chardata"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "[错误] %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		filePath      string
		pathExpr      string
		formatArg     string
		outputMode    string
		readFromStdin bool
		showFilePath  bool
	)

	flag.StringVar(&filePath, "file", "", "配置文件完整路径，例如 /etc/app.yaml")
	flag.StringVar(&pathExpr, "path", "", "配置路径，支持点语法和数组下标，例如 app.database.host 或 servers.0.port")
	flag.StringVar(&formatArg, "format", "", "配置文件格式，可选 ini|yaml|json|xml；省略时按扩展名识别")
	flag.StringVar(&outputMode, "output", "text", "输出模式，可选 text|json")
	flag.BoolVar(&readFromStdin, "stdin", false, "从标准输入读取配置内容")
	flag.BoolVar(&showFilePath, "show-file", false, "输出文件完整路径")
	flag.Usage = func() {
		printUsage(os.Stdout)
	}
	flag.Parse()

	if pathExpr == "" {
		return errors.New("--path 不能为空")
	}
	if outputMode != "text" && outputMode != "json" {
		return fmt.Errorf("--output 仅支持 text 或 json")
	}
	if readFromStdin {
		if filePath != "" {
			return errors.New("--stdin 模式下不能同时指定 --file")
		}
		if formatArg == "" {
			return errors.New("--stdin 模式下必须显式指定 --format")
		}
		return runSingleInput(formatArg, pathExpr, outputMode)
	}
	if filePath == "" {
		return errors.New("--file 不能为空")
	}
	return runSingleFile(filePath, pathExpr, formatArg, outputMode, showFilePath)
}

func runSingleInput(formatArg, pathExpr, outputMode string) error {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("读取标准输入失败: %w", err)
	}

	formatName, err := detectFormat("stdin."+formatArg, formatArg)
	if err != nil {
		return err
	}

	value, err := readConfigValueFromBytes(data, formatName, pathExpr)
	if err != nil {
		printResult(outputMode, resultRow{
			Status: "error",
			Key:    pathExpr,
			Info:   err.Error(),
			Format: formatName,
		})
		return nil
	}

	printResult(outputMode, resultRow{
		Status: "ok",
		Key:    pathExpr,
		Value:  value,
		Info:   "成功解析",
		Format: formatName,
	})
	return nil
}

type resultRow struct {
	Status string `json:"status"`
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Info   string `json:"info"`
	Error  string `json:"error,omitempty"`
	File   string `json:"file,omitempty"`
	Format string `json:"format,omitempty"`
}

func maybeFilePath(show bool, path string) string {
	if show {
		return path
	}
	return ""
}

func printResult(outputMode string, row resultRow) {
	if outputMode == "json" {
		data, err := json.Marshal(row)
		if err != nil {
			fmt.Printf("{\"status\":\"error\",\"key\":\"%s\",\"info\":\"结果序列化失败\"}\n", row.Key)
			return
		}
		fmt.Println(string(data))
		return
	}

	if row.File != "" {
		fmt.Printf("  文件: %s\n", row.File)
	}
	if row.Format != "" {
		fmt.Printf("  格式: %s\n", row.Format)
	}
	fmt.Printf("  路径: %s\n", row.Key)
	if row.Status == "ok" {
		fmt.Printf("  值: %s\n\n", row.Value)
		return
	}
	fmt.Printf("  [错误] %s\n", row.Info)
	if row.Error != "" {
		fmt.Printf("  详情: %s\n", row.Error)
	}
	fmt.Println()
}

func runSingleFile(filePath, pathExpr, formatArg, outputMode string, showFilePath bool) error {
	if _, err := os.Stat(filePath); err != nil {
		printResult(outputMode, resultRow{
			Status: "error",
			Key:    pathExpr,
			Info:   "配置文件不存在",
			Error:  filePath,
			File:   maybeFilePath(showFilePath, filePath),
		})
		return nil
	}

	formatName, err := detectFormat(filePath, formatArg)
	if err != nil {
		printResult(outputMode, resultRow{
			Status: "error",
			Key:    pathExpr,
			Info:   err.Error(),
			File:   maybeFilePath(showFilePath, filePath),
		})
		return nil
	}

	value, err := readConfigValue(filePath, formatName, pathExpr)
	if err != nil {
		printResult(outputMode, resultRow{
			Status: "error",
			Key:    pathExpr,
			Info:   err.Error(),
			File:   maybeFilePath(showFilePath, filePath),
			Format: formatName,
		})
		return nil
	}

	printResult(outputMode, resultRow{
		Status: "ok",
		Key:    pathExpr,
		Value:  value,
		Info:   "成功解析",
		File:   maybeFilePath(showFilePath, filePath),
		Format: formatName,
	})
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: get_config_value [选项]

必填选项:
  --file      <path>   配置文件完整路径，例如 /etc/app.yaml
  --path      <expr>   配置路径，支持点语法和数组下标

可选选项:
  --format    <name>   配置文件格式：ini|yaml|json|xml；默认按扩展名识别
  --output    <mode>   输出模式：text|json；默认 text
  --stdin              从标准输入读取配置内容；此时必须显式指定 --format
  --show-file          输出命中的完整文件路径
  -h, --help           显示帮助

示例:
  ./get_config_value.sh \
      --file '/data/app/conf/app.yaml' \
      --path 'server.port'

  ./get_config_value.sh \
      --file '/data/node/etc/cluster.json' \
      --path 'servers.0.host'

  ./get_config_value.sh \
      --file '/data/xml/conf/settings.xml' \
      --format xml \
      --path 'config.database.host'`)

	fmt.Fprintln(w, `
标准输入示例:
  cat /etc/app.yaml | ./get_config_value.sh --stdin --format yaml --path server.port --output json

JSON 输出示例:
  {"status":"ok","key":"aaa.bbb","value":"aaa","info":"成功解析"}`)
}

func detectFormat(path string, formatArg string) (string, error) {
	if formatArg != "" {
		switch strings.ToLower(formatArg) {
		case "ini", "yaml", "yml", "json", "xml":
			if strings.EqualFold(formatArg, "yml") {
				return "yaml", nil
			}
			return strings.ToLower(formatArg), nil
		default:
			return "", fmt.Errorf("不支持的格式: %s", formatArg)
		}
	}

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".ini", ".conf", ".cfg":
		return "ini", nil
	case ".yaml", ".yml":
		return "yaml", nil
	case ".json":
		return "json", nil
	case ".xml":
		return "xml", nil
	default:
		return "", fmt.Errorf("无法根据扩展名识别格式，请显式指定 --format")
	}
}

func readConfigValue(path, formatName, pathExpr string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}

	return readConfigValueFromBytes(data, formatName, pathExpr)
}

func readConfigValueFromBytes(data []byte, formatName, pathExpr string) (string, error) {
	var (
		root any
		err  error
	)
	switch formatName {
	case "ini":
		root, err = parseINI(data)
	case "yaml":
		root, err = parseYAML(data)
	case "json":
		root, err = parseJSON(data)
	case "xml":
		root, err = parseXML(data)
	default:
		return "", fmt.Errorf("不支持的格式: %s", formatName)
	}
	if err != nil {
		return "", err
	}

	value, err := lookupPath(root, pathExpr)
	if err != nil {
		return "", err
	}
	return stringifyValue(value), nil
}

func parseINI(data []byte) (any, error) {
	result := map[string]any{}
	current := result

	for lineNo, raw := range bytes.Split(data, []byte("\n")) {
		line := strings.TrimSpace(strings.TrimRight(string(raw), "\r"))
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSpace(line[1 : len(line)-1])
			if section == "" {
				return nil, fmt.Errorf("INI 第 %d 行 section 为空", lineNo+1)
			}
			sectionMap := map[string]any{}
			result[section] = sectionMap
			current = sectionMap
			continue
		}

		idx := strings.IndexAny(line, "=:")
		if idx < 0 {
			if strings.ContainsAny(line, " \t") {
				return nil, fmt.Errorf("INI 第 %d 行格式无效: %s", lineNo+1, line)
			}
			current[line] = true
			continue
		}

		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		if key == "" {
			return nil, fmt.Errorf("INI 第 %d 行 key 为空", lineNo+1)
		}
		current[key] = stripInlineComment(value)
	}

	return result, nil
}

func stripInlineComment(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}

	for i := 0; i < len(value); i++ {
		if value[i] == '#' || value[i] == ';' {
			if i == 0 || value[i-1] == ' ' || value[i-1] == '\t' {
				return strings.TrimSpace(value[:i])
			}
		}
	}
	return value
}

func parseYAML(data []byte) (any, error) {
	var root any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("YAML 解析失败: %w", err)
	}
	return normalizeValue(root), nil
}

func parseJSON(data []byte) (any, error) {
	var root any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	return normalizeValue(root), nil
}

func parseXML(data []byte) (any, error) {
	var node configXMLNode
	if err := xml.Unmarshal(data, &node); err != nil {
		return nil, fmt.Errorf("XML 解析失败: %w", err)
	}

	root := map[string]any{
		node.XMLName.Local: xmlNodeToValue(node),
	}
	return root, nil
}

func xmlNodeToValue(node configXMLNode) any {
	value := map[string]any{}

	for _, attr := range node.Attrs {
		value["@"+attr.Name.Local] = attr.Value
	}

	text := strings.TrimSpace(node.Text)
	if text != "" {
		value["#text"] = text
	}

	grouped := map[string][]any{}
	for _, child := range node.Nodes {
		grouped[child.XMLName.Local] = append(grouped[child.XMLName.Local], xmlNodeToValue(child))
	}

	for name, items := range grouped {
		if len(items) == 1 {
			value[name] = items[0]
		} else {
			value[name] = items
		}
	}

	if len(value) == 1 {
		if onlyText, ok := value["#text"]; ok {
			return onlyText
		}
	}

	return value
}

func normalizeValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		normalized := make(map[string]any, len(v))
		for key, child := range v {
			normalized[key] = normalizeValue(child)
		}
		return normalized
	case map[any]any:
		normalized := make(map[string]any, len(v))
		for key, child := range v {
			normalized[fmt.Sprint(key)] = normalizeValue(child)
		}
		return normalized
	case []any:
		normalized := make([]any, len(v))
		for i, child := range v {
			normalized[i] = normalizeValue(child)
		}
		return normalized
	default:
		return v
	}
}

func lookupPath(root any, expr string) (any, error) {
	parts := strings.Split(expr, ".")
	current := root

	for _, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("路径格式无效: %s", expr)
		}

		switch node := current.(type) {
		case map[string]any:
			next, ok := node[part]
			if !ok {
				return nil, fmt.Errorf("未找到路径节点: %s", part)
			}
			current = next
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("当前节点为数组，但路径片段不是数字下标: %s", part)
			}
			if index < 0 || index >= len(node) {
				return nil, fmt.Errorf("数组下标越界: %d", index)
			}
			current = node[index]
		default:
			return nil, fmt.Errorf("路径无法继续深入，片段=%s", part)
		}
	}

	return current, nil
}

func stringifyValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return v
	case bool:
		if v {
			return "true"
		}
		return "false"
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	case int, int8, int16, int32, int64:
		return fmt.Sprintf("%d", v)
	case uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", v)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(data)
	}
}
