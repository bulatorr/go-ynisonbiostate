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

	"github.com/bogem/id3v2"
	"github.com/bulatorr/go-yaynison/ynison"
	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

const (
	// номер телефона в формате +79991231212
	phone          = ""
	YM_token       = ""
	DELELE_HISTORY = false // удалить все сохраненные треки из профиля при запуске программы
	ADD_LINK       = true  // ссылка на трек в поле About профиля
)

const header = "OAuth " + YM_token

var (
	client = new(http.Client)
	bf     bytes.Buffer
)

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

	uploadClient := uploader.NewUploader(client.API())

	y := ynison.NewClient(YM_token)
	defer y.Close()

	// очистка всех сохраненных в профиле треков
	if DELELE_HISTORY {
		log.Println("Очистка истории")

		sm, err := client.API().UsersGetSavedMusic(ctx, &tg.UsersGetSavedMusicRequest{
			ID:     client.Self.AsInput(),
			Offset: 0,
			Limit:  999,
		})

		if err != nil {
			log.Fatal(err)
		}

		amSm, ok := sm.AsModified()

		if !ok {
			log.Fatal("map UsersSavedMusicClass to UsersSavedMusic failed")
		}

		for _, doc := range amSm.Documents {
			ndoc, ok := doc.AsNotEmpty()
			if !ok {
				continue
			}
			_, err := client.API().AccountSaveMusic(ctx, &tg.AccountSaveMusicRequest{
				ID:     ndoc.AsInput(),
				Unsave: true,
			})
			if err != nil {
				log.Println(err)
			}
		}
		log.Println("Очистка истории завершена")
	}

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
				// запоминаем текущий трек, чтобы не отправлять 999 одинаковых запросов в минуту
				trackid = am.PlayerState.PlayerQueue.PlayableList[am.PlayerState.PlayerQueue.CurrentPlayableIndex].PlayableID

				data, err := trackdata(am.PlayerState.PlayerQueue.PlayableList[am.PlayerState.PlayerQueue.CurrentPlayableIndex].PlayableID)
				if err != nil {
					log.Println("[worker][trackdata]", err.Error())
					return
				}

				// создаём id3v2 теги и записываем их в буфер
				tag := id3v2.NewEmptyTag()
				tag.SetTitle(data.Title)
				tag.SetArtist(data.Artists)

				n, err := tag.WriteTo(&bf)
				if err != nil {
					log.Println("[worker] tag wr", err)
					return
				}
				defer bf.Reset()

				// загружаем в телегу пустой файл с тегами
				upl := uploader.NewUpload("empty.mp3", &bf, n)

				inputFileClass, err := uploadClient.Upload(ctx, upl)

				if err != nil {
					log.Println("[worker] Upload failed", err)
					return
				}

				media := &tg.MessagesUploadMediaRequest{
					Peer: client.Self.AsInputPeer(),
					Media: &tg.InputMediaUploadedDocument{
						MimeType: "audio/mpeg",
						File:     inputFileClass,
					},
				}

				messageMedia, err := client.Client.API().MessagesUploadMedia(ctx, media)

				if err != nil {
					log.Println("[worker] MessagesUploadMedia err", err)
					return
				}
				mediaDocument, ok := messageMedia.(*tg.MessageMediaDocument)
				if !ok {
					return
				}
				document, ok := mediaDocument.Document.AsNotEmpty()
				if !ok {
					return
				}
				client.API().AccountSaveMusic(ctx, &tg.AccountSaveMusicRequest{
					ID:     document.AsInput(),
					Unsave: false,
				})

				if ADD_LINK {
					userdata, err := client.Client.Self(ctx)
					if err != nil {
						log.Println("[worker][userdata]", err)
						return
					}

					about := " "
					if !data.UploadByUser {
						about = "https://music.yandex.ru/track/" + data.ID
					}

					_, err = client.Client.API().AccountUpdateProfile(ctx, &tg.AccountUpdateProfileRequest{
						FirstName: userdata.FirstName,
						LastName:  userdata.LastName,
						About:     about,
					})
					if err != nil {
						log.Println("[worker][AccountUpdateProfile]", err)
					}
				}

				log.Printf("Сейчас играет %s: %s [%s]", data.Artists, data.Title, data.ID)
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
	mTLSConfig.MinVersion = tls.VersionTLS11
	mTLSConfig.MaxVersion = tls.VersionTLS13

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
func trackdata(trackid string) (*trackData, error) {
	req, err := http.NewRequest("GET", "https://api.music.yandex.net/tracks/"+trackid, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Authorization", header)
	req.Header.Add("x-Yandex-Music-Client", "YandexMusicAndroid/24024312")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("User-Agent", "okhttp/4.12.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		bf := new(bytes.Buffer)
		defer bf.Reset()
		bf.ReadFrom(resp.Body)
		return nil, fmt.Errorf("%d %s", resp.StatusCode, bf.String())
	}
	data := new(trackresponse)
	err = json.NewDecoder(resp.Body).Decode(data)
	if err != nil {
		return nil, err
	}
	if len(data.Result) > 0 {
		ar := ""
		for i, artist := range data.Result[0].Artists {
			if i != 0 {
				ar += ", "
			}
			ar += artist.Name
		}
		id := data.Result[0].ID

		return &trackData{
			Title:        data.Result[0].Title,
			Artists:      ar,
			ID:           id,
			UploadByUser: uuid.Validate(id) == nil,
		}, nil
	}
	return nil, fmt.Errorf("[trackdata] something went wrong")
}

type trackData struct {
	Title        string
	Artists      string
	ID           string
	UploadByUser bool
}

type trackresponse struct {
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
			// ID        int    `json:"id"`
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
