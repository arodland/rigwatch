package main

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"

	rigclient "github.com/ftl/rigproxy/pkg/client"
	rp "github.com/ftl/rigproxy/pkg/protocol"

	"github.com/rs/zerolog"
	log "github.com/rs/zerolog/log"

	"github.com/vimeo/dials/ez"
	"github.com/vimeo/dials/sources/flag"

	firebase "firebase.google.com/go/v4"
	fbdb "firebase.google.com/go/v4/db"
	"google.golang.org/api/option"
)

type Config struct {
	ConfigFile      string        `dialsdesc:"Config file"`
	Callsign        string        `dialsdesc:"Callsign" yaml:"callsign"`
	Radio           string        `dialsdesc:"Radio" yaml:"radio"`
	HamlibServer    string        `dialsdesc:"Hamlib server:port" yaml:"hamlib_server"`
	LogLevel        zerolog.Level `dialsdesc:"Log level" yaml:"log_level"`
	FirebaseProject string        `dialsdesc:"Firebase project" yaml:"firebase_project"`
	FirebaseURL     string        `dialsdesc:"Firebase URL" yaml:"firebase_url"`
	FirebaseToken   string        `dialsdesc:"Firebase token path" yaml:"firebase_token"`
}

func (c *Config) ConfigPath() (string, bool) {
	return c.ConfigFile, c.ConfigFile != ""
}

func defaultConfig() *Config {
	return &Config{
		ConfigFile:      "rigwatch.yaml",
		LogLevel:        zerolog.InfoLevel,
		HamlibServer:    "localhost:4532",
		FirebaseProject: "hrwbota",
		FirebaseURL:     "https://hrwbota-default-rtdb.firebaseio.com",
	}
}

var config *Config

type Status struct {
	Mode      string
	Frequency int64
	PTT       bool
	Updated   int64
	LastPTT   int64
}

var status Status
var prevStatus *Status

func sendStatus(ctx context.Context, status Status, fbStatus *fbdb.Ref) {
	log.Debug().Interface("status", status).Msg("new status")
	if prevStatus != nil && prevStatus.Updated >= status.Updated-4500 {
		log.Debug().Msg("skipping status update, too soon")
		return
	}
	if prevStatus != nil && prevStatus.Mode == status.Mode && prevStatus.Frequency == status.Frequency && prevStatus.PTT == status.PTT {
		if prevStatus.Updated >= status.Updated-60000 {
			log.Debug().Msg("skipping status update, no change")
			return
		} else {
			log.Debug().Msg("updating status after 60s to show we're still alive")
		}
	}
	if prevStatus == nil {
		prevStatus = &Status{}
	}
	*prevStatus = status

	err := fbStatus.Set(ctx, status)
	if err != nil {
		log.Error().Err(err).Msg("error updating firebase status")
	}
}

func main() {
	ctx := context.Background()

	log.Logger = zerolog.New(
		zerolog.ConsoleWriter{
			Out: os.Stderr,
		},
	).With().Timestamp().Logger()

	d, dErr := ez.YAMLConfigEnvFlag(ctx, defaultConfig(), ez.Params[Config]{
		FlagConfig: flag.DefaultFlagNameConfig(),
	})
	if dErr != nil {
		log.Fatal().Msgf("error loading config with dials: %s", dErr)
	}
	config = d.View()
	zerolog.SetGlobalLevel(config.LogLevel)

	opts := []option.ClientOption{}
	if config.FirebaseToken != "" {
		opts = append(opts, option.WithCredentialsFile(config.FirebaseToken))
	}

	fbApp, err := firebase.NewApp(ctx, &firebase.Config{
		DatabaseURL: config.FirebaseURL,
		ProjectID:   config.FirebaseProject,
	}, opts...)
	if err != nil {
		log.Fatal().Msgf("error initializing firebase app: %s", err)
	}

	fbDb, err := fbApp.Database(ctx)
	if err != nil {
		log.Fatal().Msgf("error initializing firebase DB: %s", err)
	}

	ham := fbDb.NewRef("hams").Child(config.Callsign)
	err = ham.Update(ctx, map[string]any{"radio": config.Radio, "source": "automatic"})
	if err != nil {
		log.Warn().Msgf("error initing ham profile: %s", err)
	}

	fbStatus := ham.Child("status")
	err = fbStatus.Get(ctx, &status)
	if err != nil {
		log.Warn().Msgf("error getting initial status: %s", err)
	}
	prevStatus = &Status{}
	*prevStatus = status

	rig, err := rigclient.Open(config.HamlibServer)
	if err != nil {
		panic(err)
	}
	defer rig.Close()
	polls := []rigclient.PollRequest{
		rigclient.PollCommandFunc(func(res rp.Response) {
			var ev *zerolog.Event
			if res.Result == "0" {
				freq, err := strconv.ParseInt(res.Data[0], 10, 64)
				if err == nil {
					ev = log.Debug()
					status.Frequency = freq
					status.Updated = time.Now().UTC().UnixMilli()
				} else {
					ev = log.Error()
				}
			} else {
				ev = log.Error()
			}
			ev.Interface("response", res).Msg("freq")
		}, "f"),
		rigclient.PollCommandFunc(func(res rp.Response) {
			var ev *zerolog.Event
			if res.Result == "0" {
				ev = log.Debug()
				status.Mode = res.Data[0]
				status.Updated = time.Now().UTC().UnixMilli()
			} else {
				ev = log.Error()
			}
			ev.Interface("response", res).Msg("mode")
		}, "m"),
		rigclient.PollCommandFunc(func(res rp.Response) {
			var ev *zerolog.Event
			if res.Result == "0" {
				ev = log.Debug()
				status.PTT = res.Data[0] != "0"
				status.Updated = time.Now().UTC().UnixMilli()
				if status.PTT {
					status.LastPTT = status.Updated
				}

				sendStatus(ctx, status, fbStatus)
			} else {
				ev = log.Error()
			}
			ev.Interface("response", res).Msg("ptt")
		}, "t"),
	}
	var wg sync.WaitGroup
	wg.Add(1)
	rig.WhenClosed(func() { wg.Done() })
	rig.StartPolling(time.Second, time.Second, polls...)
	wg.Wait()
}
