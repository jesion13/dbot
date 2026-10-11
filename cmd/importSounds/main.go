package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	dbot "dbot/pkg/bot"
	"dbot/pkg/db"
)

func main() {
	ctx := context.Background()

	db, err := db.ConnectStore(ctx, "./test.db", "")
	if err != nil {
		panic(err)
	}

	d := dbot.DBot{
		Ctx:     ctx,
		Store:   db.Queries,
		StoreDB: db,
	}

	for k, v := range parse() {
		params := dbot.SaveSoundParams{
			GID:     "492318912881491981",
			Link:    k,
			Aliases: strings.Join(v, ","),
		}
		err := d.SaveSound(ctx, params)
		if err != nil {
			panic(err)
		}
	}
}

func parse() map[string][]string {
	f, err := os.Open("./492318912881491981.json")
	if err != nil {
		panic(err)
	}

	var m map[string][]string
	err = json.NewDecoder(f).Decode(&m)
	if err != nil {
		panic(err)
	}
	return m
}
