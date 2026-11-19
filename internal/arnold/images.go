package arnold

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxImageBytes = 20 << 20
const maxImages = 8

type imagePreparationError struct{ err error }

func (e *imagePreparationError) Error() string { return "prepare Slack images: " + e.err.Error() }
func (e *imagePreparationError) Unwrap() error { return e.err }

func imageExtension(mime string) string {
	return map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}[mime]
}

func (s *Slack) DownloadImage(ctx context.Context, file SlackFile) (InputImage, error) {
	var result InputImage
	if file.ID == "" {
		return result, fmt.Errorf("Slack attachment has no file ID")
	}
	if file.URLPrivate == "" || file.MIMEType == "" {
		var response struct {
			File SlackFile `json:"file"`
		}
		if err := s.API(ctx, "files.info", map[string]string{"file": file.ID}, false, &response); err != nil {
			return result, fmt.Errorf("read Slack attachment %s: %w", file.ID, err)
		}
		if response.File.ID != file.ID {
			return result, fmt.Errorf("Slack returned a different attachment")
		}
		file = response.File
	}
	if file.External || file.Mode == "external" || imageExtension(file.MIMEType) == "" {
		return result, fmt.Errorf("Slack attachment %s is not a supported Slack-hosted image", file.ID)
	}
	if file.Size > maxImageBytes {
		return result, fmt.Errorf("Slack image %s exceeds the 20 MiB limit", file.ID)
	}
	u, err := url.Parse(file.URLPrivate)
	if err != nil || u.Scheme != "https" || u.Host != "files.slack.com" || u.User != nil {
		return result, fmt.Errorf("Slack image %s has an unsupported download host", file.ID)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return result, fmt.Errorf("invalid Slack image URL")
	}
	request.Header.Set("Authorization", "Bearer "+s.BotToken)
	client := &http.Client{Timeout: 30 * time.Second}
	if s.HTTP != nil {
		*client = *s.HTTP
	}
	// File objects and redirects are untrusted: credentials never follow them.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil
	response, err := client.Do(request)
	if err != nil {
		return result, fmt.Errorf("download Slack image %s failed", file.ID)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("download Slack image %s: HTTP %d", file.ID, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxImageBytes+1))
	if err != nil {
		return result, fmt.Errorf("read Slack image %s failed", file.ID)
	}
	if len(data) > maxImageBytes {
		return result, fmt.Errorf("Slack image %s exceeds the 20 MiB limit", file.ID)
	}
	extension := imageExtension(http.DetectContentType(data))
	if extension == "" {
		return result, fmt.Errorf("Slack attachment %s did not contain a supported image", file.ID)
	}
	return InputImage{FileID: file.ID, Name: Hash(string(data)) + extension, Data: data}, nil
}

func (a *Agent) prepareImages(ctx context.Context, chat ImageChat, files []SlackFile, target *Binding) ([]InputImage, []string, error) {
	var images []InputImage
	var notices []string
	seen := map[string]bool{}
	total := 0
	for _, file := range files {
		if file.ID == "" || seen[file.ID] {
			continue
		}
		seen[file.ID] = true
		if target != nil {
			delivered, err := a.Store.ImageDelivered(target.Task, file.ID)
			if err != nil {
				return nil, nil, err
			}
			if delivered {
				continue
			}
		}
		if file.MIMEType != "" && imageExtension(file.MIMEType) == "" || file.External || file.Mode == "external" {
			notices = append(notices, fmt.Sprintf("Slack attachment %s (%s) was not supplied: only Slack-hosted PNG, JPEG, GIF and WebP images are supported.", file.ID, strings.ReplaceAll(file.Name, "\n", " ")))
			continue
		}
		if len(images) == maxImages {
			return nil, nil, fmt.Errorf("Slack request exceeds the limit of 8 new images")
		}
		image, err := chat.DownloadImage(ctx, file)
		if err != nil {
			return nil, nil, err
		}
		total += len(image.Data)
		if total > maxImageBytes {
			return nil, nil, fmt.Errorf("Slack request images exceed the combined 20 MiB limit")
		}
		images = append(images, image)
	}
	return images, notices, nil
}
