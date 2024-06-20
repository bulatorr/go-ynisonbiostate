package main

import (
	"context"
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

const phone = "+79991234567"
const YM_token = "y0_abcdef"

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
	client := new(http.Client)
	req, _ := http.NewRequest("GET", "https://api.music.yandex.ru/tracks/"+trackid, nil)
	req.Header.Add("Authorization", "OAuth "+YM_token)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data := new(trackresponse)
	json.NewDecoder(resp.Body).Decode(data)
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
