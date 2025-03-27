package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/bulatorr/go-yaynison/ynison"
	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/glebarez/sqlite"
	"github.com/gotd/td/tg"
)

const (
	// номер телефона в формате +79991231212
	phone    = ""
	YM_token = ""
)

const header = "OAuth " + YM_token

var client = new(http.Client)

func worker(parent context.Context) {
	var trackid string
	ctx, cancel := context.WithCancel(parent)
	client, err := gotgproto.NewClient(
		12935793,
		"a2926e8cbd01ded5bed25b48cf622927",
		gotgproto.ClientTypePhone(phone),
		&gotgproto.ClientOpts{
			Session: sessionMaker.SqlSession(sqlite.Open("user.session")),
		},
	)

	if err != nil {
		log.Println("[worker][tg] failed to start client:", err)
		cancel()
		return
	}
	defer client.Stop()

	y := ynison.NewClient(YM_token)
	defer y.Close()

	log.Printf("[worker][tg] client (@%s) has been started...\n", client.Self.Username)

	y.OnConnect(func() {
		log.Println("[worker][ynison] Connected to ynison")
	})

	y.OnDisconnect(func() {
		log.Println("[worker][ynison] Disconnected from ynison", ctx.Err())
		// костыль от падения интернета
		if ctx.Err() != context.Canceled {
			time.Sleep(10 * time.Second)
			if !y.IsConnected() {
				err := y.Connect()
				if err != nil {
					cancel()
				}
			}
		}

	})

	// реагируем на обновленное состояние
	y.OnMessage(func(am ynison.PutYnisonStateResponse) {
		if len(am.PlayerState.PlayerQueue.PlayableList) > 0 {
			if trackid != am.PlayerState.PlayerQueue.PlayableList[am.PlayerState.PlayerQueue.CurrentPlayableIndex].PlayableID {
				data, err := trackdata(am.PlayerState.PlayerQueue.PlayableList[am.PlayerState.PlayerQueue.CurrentPlayableIndex].PlayableID)
				if err != nil {
					log.Println("[worker][trackdata]", err.Error())
					return
				}
				// Сейчас слушает Сектор Газа: Бомж
				log.Println(data)
				_, err = client.Client.API().AccountUpdateProfile(ctx, &tg.AccountUpdateProfileRequest{
					FirstName: client.Self.FirstName,
					LastName:  client.Self.LastName,
					About:     data,
				})
				if err != nil {
					log.Println("[worker][AccountUpdateProfile]", err.Error())
				}
				// запоминаем текущий трек, чтобы не отправлять 999 одинаковых запросов в минуту
				trackid = am.PlayerState.PlayerQueue.PlayableList[am.PlayerState.PlayerQueue.CurrentPlayableIndex].PlayableID
			}
		}
	})

	err = y.Connect()
	if err != nil {
		log.Fatalln("[worker][ynison] failed to start ynison:", err)
	}

	<-ctx.Done()
}

func main() {
	// ya captcha bypass
	mTLSConfig := &tls.Config{
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		},
	}
	mTLSConfig.MinVersion = tls.VersionTLS12
	mTLSConfig.MaxVersion = tls.VersionTLS12

	tr := &http.Transport{
		TLSClientConfig: mTLSConfig,
	}
	client.Transport = tr

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	// костыль от падения интернета x2
	ctx, mastercancel := context.WithCancel(context.Background())
	go func() {
		<-interrupt
		mastercancel()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		default:
			worker(ctx)
			if ctx.Err() != context.Canceled {
				log.Println("[main] error in worker. Restart after minute.")
				time.Sleep(time.Minute)
			}
		}
	}
}

// информация о треке
func trackdata(trackid string) (string, error) {
	req, err := http.NewRequest("GET", "https://api.music.yandex.net/tracks/"+trackid, nil)
	if err != nil {
		return "", err
	}
	req.Header.Add("Authorization", header)
	req.Header.Add("x-Yandex-Music-Client", "YandexMusicAndroid/24024312")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("User-Agent", "okhttp/4.12.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		bf := new(bytes.Buffer)
		defer bf.Reset()
		bf.ReadFrom(resp.Body)
		return "", fmt.Errorf("%d %s", resp.StatusCode, bf.String())
	}
	data := new(trackresponse)
	err = json.NewDecoder(resp.Body).Decode(data)
	if err != nil {
		return "", err
	}
	if len(data.Result) > 0 {
		ar := ""
		for i, artist := range data.Result[0].Artists {
			if i != 0 {
				ar += ", "
			}
			ar += artist.Name
		}
		return fmt.Sprintf("Сейчас слушает %s: %s", ar, data.Result[0].Title), nil
	}
	return "", fmt.Errorf("[trackdata] something went wrong")
}

type trackresponse struct {
	InvocationInfo struct {
		ReqID              string `json:"req-id"`
		Hostname           string `json:"hostname"`
		ExecDurationMillis int    `json:"exec-duration-millis"`
	} `json:"invocationInfo"`
	Result []struct {
		ID     string `json:"id"`
		RealID string `json:"realId"`
		Title  string `json:"title"`
		Major  struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"major"`
		Disclaimers       []any  `json:"disclaimers"`
		StorageDir        string `json:"storageDir"`
		DurationMs        int    `json:"durationMs"`
		FileSize          int    `json:"fileSize"`
		PreviewDurationMs int    `json:"previewDurationMs"`
		Artists           []struct {
			ID        int    `json:"id"`
			Name      string `json:"name"`
			Various   bool   `json:"various"`
			Composer  bool   `json:"composer"`
			Available bool   `json:"available"`
			Cover     struct {
				Type   string `json:"type"`
				URI    string `json:"uri"`
				Prefix string `json:"prefix"`
			} `json:"cover"`
			Genres      []any `json:"genres"`
			Disclaimers []any `json:"disclaimers"`
		} `json:"artists"`
		CoverURI        string `json:"coverUri"`
		OgImage         string `json:"ogImage"`
		LyricsAvailable bool   `json:"lyricsAvailable"`
		Type            string `json:"type"`
		TrackSource     string `json:"trackSource"`
	} `json:"result"`
}
