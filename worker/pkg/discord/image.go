package discord

import (
	"bytes"
	"context"
	"image/gif"
	"image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
)

const maxImageBytes = 7_000_000

var supportedImageTypes = map[string]struct{}{
	"image/png": {}, "image/jpeg": {}, "image/webp": {}, "image/heic": {}, "image/heif": {}, "image/gif": {},
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
		if !imageAttachmentCandidate(attachment) {
			continue
		}
		if selected == nil {
			selected = attachment
		} else if additional == nil {
			additional = attachment
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

func imageAttachmentCandidate(attachment *discordgo.MessageAttachment) bool {
	if attachment == nil {
		return false
	}
	if _, ok := supportedImageTypes[normalizedMIME(attachment.ContentType)]; ok {
		return true
	}
	if attachment.Width > 0 && attachment.Height > 0 {
		return true
	}
	switch strings.ToLower(filepath.Ext(attachment.Filename)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".heic", ".heif", ".gif":
		return true
	default:
		return false
	}
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
	data, err := io.ReadAll(io.LimitReader(response.Body, maxImageBytes+1))
	if err != nil {
		return nil, "download_failed"
	}
	if len(data) > maxImageBytes {
		return nil, "streamed_size_exceeded"
	}
	contentType := sniffImageMIME(data)
	if _, ok := supportedImageTypes[contentType]; !ok {
		return nil, "mime_mismatch"
	}
	if contentType == "image/gif" {
		data, err = firstGIFFrame(data)
		if err != nil {
			return nil, "decode_failed"
		}
		contentType = "image/png"
	}
	return &genai.Image{Data: data, MIMEType: contentType}, ""
}

func sniffImageMIME(data []byte) string {
	detected := normalizedMIME(http.DetectContentType(data))
	if _, ok := supportedImageTypes[detected]; ok {
		return detected
	}
	if len(data) >= 12 && string(data[4:8]) == "ftyp" {
		switch string(data[8:12]) {
		case "heic", "heix", "hevc", "hevx":
			return "image/heic"
		case "mif1", "msf1":
			return "image/heif"
		}
	}
	return ""
}

func firstGIFFrame(data []byte) ([]byte, error) {
	frame, err := gif.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var converted bytes.Buffer
	if err := png.Encode(&converted, frame); err != nil {
		return nil, err
	}
	return converted.Bytes(), nil
}

func visualFollowup(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" || len(strings.Fields(lower)) > 16 {
		return false
	}
	for _, phrase := range []string{
		"this image", "that image", "the image", "this photo", "that photo", "the photo",
		"this picture", "that picture", "the picture", "what it says", "what does it say",
		"read it", "where is this", "where was this", "what is this", "what's this",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func imageResearchRequested(text string) bool {
	lower := strings.ToLower(text)
	for _, phrase := range []string{"search", "look up", "verify", "identify", "where is", "where was", "price", "cost", "buy"} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func (p *Processor) previousImage(ctx context.Context, current *discordgo.Message, sections []contextSection) (*genai.Image, string) {
	for _, candidate := range priorImageCandidates(current, sections) {
		message := candidate
		if len(message.Attachments) == 0 && strings.Contains(message.Content, "[image:") {
			fetched, err := p.client.Message(ctx, message.ChannelID, message.ID)
			if err != nil || fetched == nil {
				return nil, "IMAGE ATTACHMENT NOTICE: The previous image is no longer available. Ask the user to attach it again."
			}
			message = fetched
		}
		if len(message.Attachments) == 0 {
			continue
		}
		return p.currentImage(ctx, message.Attachments)
	}
	return nil, ""
}

func priorImageCandidates(current *discordgo.Message, sections []contextSection) []*discordgo.Message {
	seen := make(map[string]struct{})
	var candidates []*discordgo.Message
	add := func(message *discordgo.Message) {
		if message == nil || message.ID == "" || message.ChannelID == "" {
			return
		}
		if _, ok := seen[message.ID]; ok {
			return
		}
		seen[message.ID] = struct{}{}
		candidates = append(candidates, message)
	}
	if current != nil && current.MessageReference != nil {
		found := false
		for _, section := range sections {
			for _, message := range section.messages {
				if message != nil && message.ID == current.MessageReference.MessageID {
					add(message)
					found = true
				}
			}
		}
		if !found {
			channelID := current.MessageReference.ChannelID
			if channelID == "" {
				channelID = current.ChannelID
			}
			add(&discordgo.Message{ID: current.MessageReference.MessageID, ChannelID: channelID, Content: "[image: referenced attachment]"})
		}
	}
	for _, section := range sections {
		if section.label == "PARENT CHANNEL" {
			continue
		}
		for i := len(section.messages) - 1; i >= 0; i-- {
			message := section.messages[i]
			if message != nil && (len(message.Attachments) > 0 || strings.Contains(message.Content, "[image:")) {
				add(message)
			}
		}
	}
	return candidates
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
