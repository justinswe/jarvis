package discord

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
)

const maxImageBytes = 7_000_000

var supportedImageTypes = map[string]struct{}{
	"image/png": {}, "image/jpeg": {}, "image/webp": {}, "image/heic": {}, "image/heif": {},
}

func newImageHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if !allowedImageURL(req.URL) {
				return errors.New("redirect_not_allowed")
			}
			return nil
		},
	}
}

func allowedImageURL(value *url.URL) bool {
	if value == nil || value.Scheme != "https" || value.User != nil {
		return false
	}
	if port := value.Port(); port != "" && port != "443" {
		return false
	}
	switch strings.ToLower(value.Hostname()) {
	case "cdn.discordapp.com", "media.discordapp.net":
		return true
	default:
		return false
	}
}

func (p *Processor) currentImage(ctx context.Context, attachments []*discordgo.MessageAttachment) (*genai.Image, string) {
	if len(attachments) == 0 {
		return nil, ""
	}
	var selected *discordgo.MessageAttachment
	var additional *discordgo.MessageAttachment
	for _, attachment := range attachments {
		if attachment == nil {
			continue
		}
		if _, ok := supportedImageTypes[normalizedMIME(attachment.ContentType)]; ok {
			if selected == nil {
				selected = attachment
			} else if additional == nil {
				additional = attachment
			}
		}
	}
	if selected == nil {
		return nil, imageNotice(attachments[0], "unsupported_format")
	}
	image, code := p.downloadImage(ctx, selected)
	if code != "" {
		app.L().Info("Image attachment rejected", zap.Int("image_count", len(attachments)), zap.String("rejection_code", code))
		return nil, imageNotice(selected, code)
	}
	app.L().Info("Image attachment accepted", zap.Int("image_count", len(attachments)), zap.Int("accepted_bytes", len(image.Data)))
	if additional != nil {
		return image, imageNotice(additional, "one_image_limit")
	}
	return image, ""
}

func (p *Processor) downloadImage(ctx context.Context, attachment *discordgo.MessageAttachment) (*genai.Image, string) {
	if attachment.Size > maxImageBytes {
		return nil, "declared_size_exceeded"
	}
	parsed, err := url.Parse(attachment.URL)
	if err != nil || !allowedImageURL(parsed) {
		return nil, "url_not_allowed"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "download_failed"
	}
	client := p.imageClient
	if client == nil {
		client = newImageHTTPClient()
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "cancelled"
		}
		return nil, "download_failed"
	}
	defer response.Body.Close()
	if response.Request == nil || !allowedImageURL(response.Request.URL) {
		return nil, "redirect_not_allowed"
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "bad_status"
	}
	if response.ContentLength > maxImageBytes {
		return nil, "declared_size_exceeded"
	}
	contentType := normalizedMIME(response.Header.Get("Content-Type"))
	if _, ok := supportedImageTypes[contentType]; !ok || contentType != normalizedMIME(attachment.ContentType) {
		return nil, "mime_mismatch"
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxImageBytes+1))
	if err != nil {
		return nil, "download_failed"
	}
	if len(data) > maxImageBytes {
		return nil, "streamed_size_exceeded"
	}
	return &genai.Image{Data: data, MIMEType: contentType}, ""
}

func normalizedMIME(value string) string {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return strings.ToLower(mediaType)
}

func imageNotice(attachment *discordgo.MessageAttachment, reason string) string {
	name := "attachment"
	if attachment != nil {
		name = filepath.Base(strings.Map(func(r rune) rune {
			if r < 32 || r == 127 {
				return -1
			}
			return r
		}, attachment.Filename))
		if name == "." || name == "" {
			name = "attachment"
		}
	}
	runes := []rune(name)
	if len(runes) > 128 {
		name = string(runes[:128])
	}
	return "IMAGE ATTACHMENT NOTICE: " + name + " was not available for viewing (" + reason + "). Do not claim that you viewed unavailable images."
}
