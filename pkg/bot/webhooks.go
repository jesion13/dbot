package dbot

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

const DbotHook = "dbot/hook"

func (b *DBot) GetWebHook(ctx context.Context, chID snowflake.ID, name, avatarURL string) (*discord.IncomingWebhook, error) {
	hooks, err := b.Client.Rest.GetWebhooks(chID)
	if err != nil {
		return nil, err
	}

	for _, hook := range hooks {
		if hook.Name() == name {
			if incoming, ok := hook.(discord.IncomingWebhook); ok {
				return &incoming, nil
			}
		}
	}

	create := discord.WebhookCreate{Name: name}
	if avatarURL != "" {
		icon, err := imageData(avatarURL)
		if err != nil {
			return nil, err
		}
		create.Avatar = icon
	}

	return b.Client.Rest.CreateWebhook(chID, create)
}

func imageData(url string) (*discord.Icon, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	iconType := discord.IconType(http.DetectContentType(data))
	icon, err := discord.NewIcon(iconType, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to build icon: %w", err)
	}
	return icon, nil
}
