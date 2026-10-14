package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const protocolBodyLimit int64 = 64 << 20

// validateInput 将兼容接口参数转换为页面已确认的模型能力，不能静默忽略不支持的比例。
func validateInput(input *GenerateInput) error {
	if input.Model == "" {
		input.Model = availableModels[0].ID
	}
	model, ok := resolveModel(input.Model)
	if !ok {
		return errors.New("模型不存在，请从模型目录选择")
	}
	input.Model = model.ID
	input.Prompt = strings.TrimSpace(input.Prompt)
	if input.Prompt == "" || len(input.Prompt) > 32768 {
		return errors.New("提示词不能为空或超过 32768 字节")
	}
	if input.N == 0 {
		input.N = 1
	}
	if input.N < 1 || input.N > 4 {
		return errors.New("生成数量应为 1 至 4")
	}
	if input.Quality == "" {
		input.Quality = "auto"
	}
	if !hasString([]string{"auto", "low", "medium", "high"}, input.Quality) {
		return errors.New("画质应为 auto、low、medium 或 high")
	}
	ratio, resolution, _, _, err := adobeParameters(model, *input)
	if err != nil {
		return err
	}
	input.AspectRatio, input.Resolution = ratio, resolution
	limit := 6
	if model.Version == "nano-banana" {
		limit = 4
	}
	if len(input.References) > limit {
		return fmt.Errorf("所选模型最多支持 %d 张参考图", limit)
	}
	return nil
}

func decodeProtocolJSON(w http.ResponseWriter, r *http.Request, value any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("请使用 application/json 请求")
	}
	r.Body = http.MaxBytesReader(w, r.Body, protocolBodyLimit)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(value); err != nil {
		return errors.New("请求内容无效或超过 64 MB")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("请求只允许一个 JSON 对象")
	}
	return nil
}

func (a *App) imageURL(r *http.Request, image ImageRecord) string {
	base := strings.TrimRight(a.store.Settings().PublicURL, "/")
	if base == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	return base + "/images/" + url.PathEscape(image.ID)
}

func (a *App) canonicalReferences(input *GenerateInput) error {
	if len(input.References) > 6 {
		return errors.New("最多支持 6 张参考图")
	}
	var total int64
	for i, reference := range input.References {
		image, err := a.store.Image(reference.ID)
		if err != nil || image.Deleted {
			return errors.New("参考图不存在或已清理，请重新上传")
		}
		input.References[i] = image
		total += image.Bytes
	}
	if total > protocolBodyLimit {
		return errors.New("参考图总大小不能超过 64 MB")
	}
	return nil
}

func appendReference(input *GenerateInput, image ImageRecord) error {
	var total int64 = image.Bytes
	for _, reference := range input.References {
		total += reference.Bytes
	}
	if len(input.References) >= 6 || total > protocolBodyLimit {
		return errors.New("最多支持 6 张参考图，合计不能超过 64 MB")
	}
	input.References = append(input.References, image)
	return nil
}

// saveReference 只读取短文件头识别格式，图片本体由存储层流式写入磁盘。
func (a *App) saveReference(reader io.Reader, declared string) (ImageRecord, error) {
	buffered := bufio.NewReaderSize(reader, 512)
	head, err := buffered.Peek(512)
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		return ImageRecord{}, err
	}
	actual := http.DetectContentType(head)
	if len(head) >= 12 && string(head[4:8]) == "ftyp" && (strings.Contains(string(head[8:min(len(head), 32)]), "avif") || strings.Contains(string(head[8:min(len(head), 32)]), "avis")) {
		actual = "image/avif"
	}
	if !hasString([]string{"image/png", "image/jpeg", "image/webp", "image/gif", "image/avif"}, actual) {
		return ImageRecord{}, errors.New("参考图格式无效，仅支持 PNG、JPEG、WebP、GIF、AVIF")
	}
	if declared != "" {
		requested, _, parseErr := mime.ParseMediaType(declared)
		if parseErr != nil || (requested != "application/octet-stream" && requested != actual) {
			return ImageRecord{}, errors.New("参考图 MIME 类型与文件内容不一致")
		}
	}
	return a.store.SaveImage(buffered, actual, "reference")
}

func (a *App) localReference(r *http.Request, raw string) (ImageRecord, bool, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ImageRecord{}, false, err
	}
	if parsed.User != nil || parsed.Fragment != "" || (parsed.Scheme != "" && parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ImageRecord{}, false, errors.New("图片地址无效")
	}
	local := parsed.Host == "" || strings.EqualFold(parsed.Host, r.Host)
	public, _ := url.Parse(a.store.Settings().PublicURL)
	if public != nil && public.Host != "" && strings.EqualFold(parsed.Host, public.Host) {
		local = true
	}
	path := parsed.Path
	if public != nil && public.Path != "" && strings.HasPrefix(path, strings.TrimRight(public.Path, "/")+"/images/") {
		path = strings.TrimPrefix(path, strings.TrimRight(public.Path, "/"))
	}
	if !local || !strings.HasPrefix(path, "/images/") {
		return ImageRecord{}, false, nil
	}
	id := strings.TrimPrefix(path, "/images/")
	if id == "" || strings.Contains(id, "/") {
		return ImageRecord{}, true, errors.New("图片地址无效")
	}
	image, err := a.store.Image(id)
	if err != nil || image.Deleted {
		return ImageRecord{}, true, errors.New("参考图不存在或已清理")
	}
	return image, true, nil
}

var referenceForbiddenRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("::/96"), netip.MustParsePrefix("2001::/32"),
}

func publicReferenceIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	for _, prefix := range referenceForbiddenRanges {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
func validReferenceURL(parsed *url.URL) bool {
	return parsed != nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Hostname() != "" && parsed.User == nil && parsed.Fragment == ""
}

// 每次建立连接先验证 DNS 的全部结果，再固定拨号到通过检查的地址，防止 DNS 重绑定。
func referenceClient() *http.Client {
	transport := &http.Transport{TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second, DisableKeepAlives: true, MaxResponseHeaderBytes: 32 << 10}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(addresses) == 0 {
			return nil, errors.New("参考图地址无法解析")
		}
		if len(addresses) > 32 {
			return nil, errors.New("参考图地址解析结果过多")
		}
		for _, ip := range addresses {
			if !publicReferenceIP(ip) {
				return nil, errors.New("参考图地址不能指向内网或保留地址")
			}
		}
		dialer := net.Dialer{Timeout: 10 * time.Second}
		var last error
		for _, ip := range addresses {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			last = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, last
	}
	return &http.Client{Transport: transport, Timeout: 60 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 || !validReferenceURL(req.URL) {
			return errors.New("参考图重定向地址无效")
		}
		return nil
	}}
}

func (a *App) referenceURL(r *http.Request, raw string) (ImageRecord, error) {
	if len(raw) > int(protocolBodyLimit) {
		return ImageRecord{}, errors.New("参考图编码超过大小限制")
	}
	if strings.HasPrefix(raw, "data:") {
		header, content, ok := strings.Cut(strings.TrimPrefix(raw, "data:"), ",")
		if !ok || !strings.HasSuffix(header, ";base64") {
			return ImageRecord{}, errors.New("参考图 Data URL 必须使用 Base64 编码")
		}
		return a.saveReference(base64.NewDecoder(base64.StdEncoding, strings.NewReader(content)), strings.TrimSuffix(header, ";base64"))
	}
	if image, local, err := a.localReference(r, raw); local || err != nil {
		return image, err
	}
	parsed, err := url.Parse(raw)
	if err != nil || !validReferenceURL(parsed) {
		return ImageRecord{}, errors.New("参考图应为有效的 HTTP 地址或 Data URL")
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, parsed.String(), nil)
	if err != nil {
		return ImageRecord{}, errors.New("参考图地址无效")
	}
	response, err := referenceClient().Do(request)
	if err != nil {
		return ImageRecord{}, errors.New("参考图下载失败或地址不允许访问")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ImageRecord{}, errors.New("参考图下载失败")
	}
	if response.ContentLength > maxImageBytes {
		return ImageRecord{}, errors.New("参考图超过 64 MB")
	}
	return a.saveReference(io.LimitReader(response.Body, maxImageBytes+1), response.Header.Get("Content-Type"))
}

func (a *App) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, protocolBodyLimit)
	defer r.Body.Close()
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, 400, "请使用 multipart/form-data 上传图片")
		return
	}
	var image ImageRecord
	files := 0
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeError(w, 400, "图片上传无效或超过 64 MB")
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			part.Close()
			writeError(w, 400, "请通过 file 字段上传图片")
			return
		}
		files++
		if files > 1 {
			part.Close()
			writeError(w, 400, "每次只能上传一张图片")
			return
		}
		image, err = a.saveReference(part, part.Header.Get("Content-Type"))
		part.Close()
		if err != nil {
			writeError(w, 400, safeError(err))
			return
		}
	}
	if files == 0 {
		writeError(w, 400, "请选择要上传的图片")
		return
	}
	image.URL = a.imageURL(r, image)
	writeJSON(w, 200, image)
}

func (a *App) adminGenerate(w http.ResponseWriter, r *http.Request) {
	var input GenerateInput
	if err := decodeProtocolJSON(w, r, &input); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := a.canonicalReferences(&input); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	result, entry, err := a.runGenerate(r.Context(), input, "admin", r.URL.Path)
	if err != nil {
		writeError(w, entry.HTTPStatus, safeError(err))
		return
	}
	images := append([]ImageRecord(nil), result.Images...)
	for i := range images {
		images[i].URL = a.imageURL(r, images[i])
	}
	writeJSON(w, 200, map[string]any{"id": entry.ID, "images": images, "credits": result.Credits, "duration_ms": entry.DurationMS})
}

func (a *App) openAIModels(w http.ResponseWriter, r *http.Request) {
	items := make([]map[string]any, 0, len(availableModels))
	for _, model := range availableModels {
		items = append(items, map[string]any{"id": model.ID, "object": "model", "created": 0, "owned_by": "adobe", "name": model.Name})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": items})
}
func (a *App) geminiModels(w http.ResponseWriter, r *http.Request) {
	items := make([]map[string]any, 0, len(availableModels))
	for _, model := range availableModels {
		items = append(items, map[string]any{"name": "models/" + model.ID, "displayName": model.Name, "version": model.Version, "supportedGenerationMethods": []string{"generateContent", "streamGenerateContent"}})
	}
	writeJSON(w, 200, map[string]any{"models": items})
}

type imageURLInput struct {
	ImageURL string `json:"image_url"`
}
type imageProtocolInput struct {
	GenerateInput
	ResponseFormat string          `json:"response_format"`
	Images         []imageURLInput `json:"images"`
	Image          json.RawMessage `json:"image"`
	Mask           json.RawMessage `json:"mask"`
	OutputFormat   string          `json:"output_format"`
}

func (a *App) imageJSON(w http.ResponseWriter, r *http.Request) (imageProtocolInput, error) {
	var input imageProtocolInput
	if err := decodeProtocolJSON(w, r, &input); err != nil {
		return input, err
	}
	if len(input.Mask) > 0 && string(input.Mask) != "null" {
		return input, errors.New("当前模型不支持遮罩编辑")
	}
	if input.OutputFormat != "" {
		return input, errors.New("当前接口不支持指定输出文件格式")
	}
	if err := a.canonicalReferences(&input.GenerateInput); err != nil {
		return input, err
	}
	if len(input.Image) > 0 && string(input.Image) != "null" {
		var raw string
		if err := json.Unmarshal(input.Image, &raw); err != nil {
			return input, errors.New("image 字段应为参考图地址")
		}
		input.Images = append(input.Images, imageURLInput{ImageURL: raw})
	}
	// 原始 JSON 编码已转换为地址列表，尽早解除较大的 RawMessage 引用。
	input.Image = nil
	if len(input.Images)+len(input.References) > 6 {
		return input, errors.New("最多支持 6 张参考图")
	}
	for index, reference := range input.Images {
		image, err := a.referenceURL(r, reference.ImageURL)
		if err != nil {
			return input, err
		}
		if err := appendReference(&input.GenerateInput, image); err != nil {
			return input, err
		}
		input.Images[index].ImageURL = ""
	}
	// 生图等待期间只保留磁盘索引，不让请求结构继续持有 Data URL 编码。
	input.Images = nil
	return input, nil
}
func validResponseFormat(input *imageProtocolInput) error {
	if input.ResponseFormat == "" {
		input.ResponseFormat = "url"
	}
	if input.ResponseFormat != "url" && input.ResponseFormat != "b64_json" {
		return errors.New("response_format 应为 url 或 b64_json")
	}
	return validateInput(&input.GenerateInput)
}
func (a *App) openAIImages(w http.ResponseWriter, r *http.Request) {
	input, err := a.imageJSON(w, r)
	if err == nil {
		err = validResponseFormat(&input)
	}
	if err != nil {
		writeError(w, 400, safeError(err))
		return
	}
	a.generateOpenAIImages(w, r, input)
}
func (a *App) openAIEdits(w http.ResponseWriter, r *http.Request) {
	var input imageProtocolInput
	var err error
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "multipart/form-data" {
		input, err = a.imageMultipart(w, r)
	} else {
		input, err = a.imageJSON(w, r)
	}
	if err == nil && len(input.References) == 0 {
		err = errors.New("编辑请求至少需要一张参考图")
	}
	if err == nil {
		err = validResponseFormat(&input)
	}
	if err != nil {
		writeError(w, 400, safeError(err))
		return
	}
	a.generateOpenAIImages(w, r, input)
}
func (a *App) imageMultipart(w http.ResponseWriter, r *http.Request) (imageProtocolInput, error) {
	var input imageProtocolInput
	r.Body = http.MaxBytesReader(w, r.Body, protocolBodyLimit)
	defer r.Body.Close()
	reader, err := r.MultipartReader()
	if err != nil {
		return input, errors.New("图片编辑请求格式无效")
	}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return input, errors.New("图片编辑请求无效或超过 64 MB")
		}
		name := part.FormName()
		if name == "mask" {
			part.Close()
			return input, errors.New("当前模型不支持遮罩编辑")
		}
		if part.FileName() != "" {
			if name != "image" && name != "image[]" {
				part.Close()
				return input, errors.New("参考图文件字段应为 image 或 image[]")
			}
			if len(input.References) >= 6 {
				part.Close()
				return input, errors.New("最多支持 6 张参考图")
			}
			image, err := a.saveReference(part, part.Header.Get("Content-Type"))
			part.Close()
			if err != nil {
				return input, err
			}
			if err := appendReference(&input.GenerateInput, image); err != nil {
				return input, err
			}
			continue
		}
		content, err := io.ReadAll(io.LimitReader(part, 32769))
		part.Close()
		if err != nil || len(content) > 32768 {
			return input, errors.New("表单参数无效或过长")
		}
		value := string(content)
		switch name {
		case "model":
			input.Model = value
		case "prompt":
			input.Prompt = value
		case "size":
			input.Size = value
		case "aspect_ratio":
			input.AspectRatio = value
		case "resolution":
			input.Resolution = value
		case "quality":
			input.Quality = value
		case "response_format":
			input.ResponseFormat = value
		case "n":
			input.N, err = strconv.Atoi(value)
			if err != nil {
				return input, errors.New("生成数量无效")
			}
		case "output_format":
			if value != "" {
				return input, errors.New("当前接口不支持指定输出文件格式")
			}
		case "image", "image[]":
			return input, errors.New("参考图应上传为文件")
		}
	}
	return input, nil
}

// 打开并固定返回图片后再写响应头，避免清理线程使编码响应中途损坏。
func (a *App) openImageFiles(images []ImageRecord) ([]*os.File, func(), error) {
	release := a.store.PinImages(images)
	files := make([]*os.File, 0, len(images))
	closeAll := func() {
		for _, file := range files {
			file.Close()
		}
		release()
	}
	for _, image := range images {
		record, err := a.store.Image(image.ID)
		if err != nil || record.Deleted {
			closeAll()
			return nil, func() {}, errors.New("生成图片已清理")
		}
		file, err := os.Open(record.Path)
		if err != nil {
			closeAll()
			return nil, func() {}, errors.New("生成图片无法读取")
		}
		files = append(files, file)
	}
	return files, closeAll, nil
}
func jsonString(value string) string { data, _ := json.Marshal(value); return string(data) }
func streamBase64(w io.Writer, file *os.File) error {
	encoder := base64.NewEncoder(base64.StdEncoding, w)
	_, err := io.CopyBuffer(encoder, file, make([]byte, 32<<10))
	closeErr := encoder.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func (a *App) generateOpenAIImages(w http.ResponseWriter, r *http.Request, input imageProtocolInput) {
	result, entry, err := a.runGenerate(r.Context(), input.GenerateInput, "openai", r.URL.Path)
	if err != nil {
		writeError(w, entry.HTTPStatus, safeError(err))
		return
	}
	if input.ResponseFormat == "url" {
		items := make([]map[string]string, 0, len(result.Images))
		for _, image := range result.Images {
			items = append(items, map[string]string{"url": a.imageURL(r, image)})
		}
		writeJSON(w, 200, map[string]any{"created": entry.CreatedAt.Unix(), "data": items})
		return
	}
	files, closeAll, err := a.openImageFiles(result.Images)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer closeAll()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	fmt.Fprintf(w, `{"created":%d,"data":[`, entry.CreatedAt.Unix())
	for i, file := range files {
		if i > 0 {
			io.WriteString(w, ",")
		}
		io.WriteString(w, `{"b64_json":"`)
		if err := streamBase64(w, file); err != nil {
			return
		}
		io.WriteString(w, `"}`)
	}
	io.WriteString(w, "]}")
}

type chatPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}
type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}
type chatProtocolInput struct {
	GenerateInput
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

func (a *App) openAIChat(w http.ResponseWriter, r *http.Request) {
	var request chatProtocolInput
	if err := decodeProtocolJSON(w, r, &request); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if len(request.Messages) == 0 || len(request.Messages) > 100 {
		writeError(w, 400, "请提供 1 至 100 条聊天消息")
		return
	}
	texts := make([]string, 0, len(request.Messages))
	references := make([]string, 0)
	for _, message := range request.Messages {
		if message.Role != "user" && message.Role != "system" && message.Role != "developer" && message.Role != "assistant" {
			writeError(w, 400, "不支持该聊天角色")
			return
		}
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			if text != "" {
				texts = append(texts, text)
			}
			continue
		}
		var parts []chatPart
		if json.Unmarshal(message.Content, &parts) != nil {
			writeError(w, 400, "聊天消息内容无效")
			return
		}
		for _, part := range parts {
			switch part.Type {
			case "text":
				texts = append(texts, part.Text)
			case "image_url":
				references = append(references, part.ImageURL.URL)
			default:
				writeError(w, 400, "仅支持文字和参考图消息")
				return
			}
		}
	}
	request.Prompt = strings.Join(texts, "\n\n")
	// 完成内容提取后清空原消息，后续响应闭包只使用规范化提示词和模型信息。
	request.Messages = nil
	texts = nil
	if len(references)+len(request.References) > 6 {
		writeError(w, 400, "最多支持 6 张参考图")
		return
	}
	if err := a.canonicalReferences(&request.GenerateInput); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	for index, raw := range references {
		image, err := a.referenceURL(r, raw)
		if err != nil {
			writeError(w, 400, safeError(err))
			return
		}
		if err := appendReference(&request.GenerateInput, image); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		references[index] = ""
	}
	references = nil
	if err := validateInput(&request.GenerateInput); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	complete := func(result GenerateResult, entry RequestLog) {
		links := make([]string, 0, len(result.Images))
		for _, image := range result.Images {
			links = append(links, "![生成图片]("+a.imageURL(r, image)+")")
		}
		text := strings.Join(links, "\n\n")
		id := "chatcmpl-" + entry.ID
		if !request.Stream {
			writeJSON(w, 200, map[string]any{"id": id, "object": "chat.completion", "created": entry.CreatedAt.Unix(), "model": request.Model, "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": text}, "finish_reason": "stop"}}})
			return
		}
		writeSSEJSON(w, map[string]any{"id": id, "object": "chat.completion.chunk", "created": entry.CreatedAt.Unix(), "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"role": "assistant", "content": text}, "finish_reason": nil}}})
		writeSSEJSON(w, map[string]any{"id": id, "object": "chat.completion.chunk", "created": entry.CreatedAt.Unix(), "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
		io.WriteString(w, "data: [DONE]\n\n")
		flushSSE(w)
	}
	if request.Stream {
		a.streamGenerate(w, r, request.GenerateInput, "openai", complete)
		return
	}
	result, entry, err := a.runGenerate(r.Context(), request.GenerateInput, "openai", r.URL.Path)
	if err != nil {
		writeError(w, entry.HTTPStatus, safeError(err))
		return
	}
	complete(result, entry)
}

func flushSSE(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
func writeSSEJSON(w http.ResponseWriter, value any) {
	io.WriteString(w, "data: ")
	json.NewEncoder(w).Encode(value)
	io.WriteString(w, "\n")
	flushSSE(w)
}
func geminiErrorValue(status int, message string) map[string]any {
	label := map[int]string{400: "INVALID_ARGUMENT", 401: "UNAUTHENTICATED", 403: "PERMISSION_DENIED", 404: "NOT_FOUND", 429: "RESOURCE_EXHAUSTED", 499: "CANCELLED", 500: "INTERNAL", 502: "UNAVAILABLE", 503: "UNAVAILABLE", 504: "DEADLINE_EXCEEDED"}[status]
	if label == "" {
		label = "UNKNOWN"
	}
	return map[string]any{"error": map[string]any{"code": status, "message": message, "status": label}}
}
func writeGeminiError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, geminiErrorValue(status, message))
}

// SSE 等待期间仅发心跳；只有上游真正完成后才返回图片，不伪造中间生成结果。
func (a *App) streamGenerate(w http.ResponseWriter, r *http.Request, input GenerateInput, protocol string, complete func(GenerateResult, RequestLog)) {
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, 500, "当前连接不支持流式响应")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flushSSE(w)
	type generation struct {
		result GenerateResult
		entry  RequestLog
		err    error
	}
	done := make(chan generation, 1)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	a.jobs.Add(1)
	go func() {
		defer a.jobs.Done()
		result, entry, err := a.runGenerate(ctx, input, protocol, r.URL.Path)
		done <- generation{result, entry, err}
	}()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flushSSE(w)
		case generated := <-done:
			if generated.err != nil {
				value := geminiErrorValue(generated.entry.HTTPStatus, safeError(generated.err))
				if protocol == "openai" {
					value = map[string]any{"error": map[string]any{"message": safeError(generated.err), "type": "api_error", "code": generated.entry.HTTPStatus}}
				}
				io.WriteString(w, "event: error\n")
				writeSSEJSON(w, value)
				if protocol == "openai" {
					io.WriteString(w, "data: [DONE]\n\n")
					flushSSE(w)
				}
				return
			}
			complete(generated.result, generated.entry)
			return
		}
	}
}

type geminiPart struct {
	Text       string `json:"text"`
	InlineData *struct {
		MIME string `json:"mimeType"`
		Data string `json:"data"`
	} `json:"inlineData"`
	FileData *struct {
		MIME string `json:"mimeType"`
		URI  string `json:"fileUri"`
	} `json:"fileData"`
}
type geminiProtocolInput struct {
	Contents []struct {
		Role  string       `json:"role"`
		Parts []geminiPart `json:"parts"`
	} `json:"contents"`
	SystemInstruction *struct {
		Parts []geminiPart `json:"parts"`
	} `json:"systemInstruction"`
	GenerationConfig struct {
		CandidateCount     int      `json:"candidateCount"`
		ResponseModalities []string `json:"responseModalities"`
		ImageConfig        struct {
			AspectRatio string `json:"aspectRatio"`
			ImageSize   string `json:"imageSize"`
		} `json:"imageConfig"`
	} `json:"generationConfig"`
}

func (a *App) geminiGenerate(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	model, method, ok := strings.Cut(action, ":")
	if !ok || (method != "generateContent" && method != "streamGenerateContent") {
		writeGeminiError(w, 404, "接口不存在")
		return
	}
	var request geminiProtocolInput
	if err := decodeProtocolJSON(w, r, &request); err != nil {
		writeGeminiError(w, 400, err.Error())
		return
	}
	if len(request.Contents) == 0 || len(request.Contents) > 100 {
		writeGeminiError(w, 400, "请提供 1 至 100 条生成内容")
		return
	}
	if len(request.GenerationConfig.ResponseModalities) > 0 && !hasString(request.GenerationConfig.ResponseModalities, "IMAGE") {
		writeGeminiError(w, 400, "当前接口用于图片生成，请包含 IMAGE 响应类型")
		return
	}
	input := GenerateInput{Model: model, N: request.GenerationConfig.CandidateCount, AspectRatio: request.GenerationConfig.ImageConfig.AspectRatio, Resolution: request.GenerationConfig.ImageConfig.ImageSize}
	texts := []string{}
	parts := []geminiPart{}
	if request.SystemInstruction != nil {
		parts = append(parts, request.SystemInstruction.Parts...)
	}
	for _, content := range request.Contents {
		if content.Role != "" && content.Role != "user" && content.Role != "model" {
			writeGeminiError(w, 400, "生成内容角色无效")
			return
		}
		parts = append(parts, content.Parts...)
	}
	// 配置和内容已转移到规范化输入，清空原请求避免隐式保留完整图片编码。
	request = geminiProtocolInput{}
	for index, part := range parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
		if part.InlineData != nil || part.FileData != nil {
			if len(input.References) >= 6 {
				writeGeminiError(w, 400, "最多支持 6 张参考图")
				return
			}
			var image ImageRecord
			var err error
			if part.InlineData != nil {
				image, err = a.saveReference(base64.NewDecoder(base64.StdEncoding, strings.NewReader(part.InlineData.Data)), part.InlineData.MIME)
			} else {
				image, err = a.referenceURL(r, part.FileData.URI)
			}
			if err != nil {
				writeGeminiError(w, 400, safeError(err))
				return
			}
			if err := appendReference(&input, image); err != nil {
				writeGeminiError(w, 400, err.Error())
				return
			}
		}
		parts[index] = geminiPart{}
	}
	input.Prompt = strings.Join(texts, "\n\n")
	parts = nil
	texts = nil
	if err := validateInput(&input); err != nil {
		writeGeminiError(w, 400, err.Error())
		return
	}
	stream := method == "streamGenerateContent"
	complete := func(result GenerateResult, entry RequestLog) {
		files, closeAll, err := a.openImageFiles(result.Images)
		if err != nil {
			if stream {
				io.WriteString(w, "event: error\n")
				writeSSEJSON(w, geminiErrorValue(500, err.Error()))
			} else {
				writeGeminiError(w, 500, err.Error())
			}
			return
		}
		defer closeAll()
		if stream {
			io.WriteString(w, "data: ")
		} else {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		io.WriteString(w, `{"candidates":[{"index":0,"content":{"role":"model","parts":[`)
		for i, file := range files {
			if i > 0 {
				io.WriteString(w, ",")
			}
			fmt.Fprintf(w, `{"inlineData":{"mimeType":%s,"data":"`, jsonString(result.Images[i].MIME))
			if err := streamBase64(w, file); err != nil {
				return
			}
			io.WriteString(w, `"}}`)
		}
		fmt.Fprintf(w, `]},"finishReason":"STOP"}],"modelVersion":%s,"responseId":%s}`, jsonString(input.Model), jsonString(entry.ID))
		if stream {
			io.WriteString(w, "\n\n")
			flushSSE(w)
		}
	}
	if stream {
		a.streamGenerate(w, r, input, "gemini", complete)
		return
	}
	result, entry, err := a.runGenerate(r.Context(), input, "gemini", r.URL.Path)
	if err != nil {
		writeGeminiError(w, entry.HTTPStatus, safeError(err))
		return
	}
	complete(result, entry)
}

func (a *App) serveImage(w http.ResponseWriter, r *http.Request) {
	image, err := a.store.Image(r.PathValue("id"))
	if err != nil || image.Deleted {
		writeError(w, 404, "图片不存在或已清理")
		return
	}
	unpin := a.store.PinImages([]ImageRecord{image})
	defer unpin()
	file, err := os.Open(image.Path)
	if err != nil {
		writeError(w, 404, "图片不存在或已清理")
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", image.MIME)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("ETag", `"`+image.ID+`"`)
	http.ServeContent(w, r, image.ID, image.CreatedAt, file)
}
