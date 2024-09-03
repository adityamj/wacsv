// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bytes"
	"encoding/csv"
	"io"
	"math/rand"
	"regexp"
	"text/template"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"
	"github.com/nfnt/resize"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

var cli *whatsmeow.Client
var log waLog.Logger

var logLevel = "INFO"
var debugLogs = flag.Bool("debug", false, "Enable debug logs?")
var dbDialect = flag.String("db-dialect", "sqlite3", "Database dialect (sqlite3 or postgres)")
var dbAddress = flag.String("db-address", "mdtest.db", "Database address or file location in case of sqlite3")
var requestFullSync = flag.Bool("request-full-sync", false, "Request full (1 year) history sync when logging in?")
var messageFile = flag.String("input", "", "Input CSV, column 0 should contain mobile number or group jid. If not, -m should be used to specify the correct column")
var mediaPath = flag.String("media", "", "Media for attachment, will be sent as document type by default unless overriden by -img or -vid")
var thumbPath = flag.String("thumb", "", "Thumbnail for the media. Needed for video or pdf media types")
var fn = flag.String("sent-name", "", "Rename the file name to this for document type messages")
var sleepDuration = flag.Int("sleep", 3000, "Sleep Duration in ms between successive messages in milliseconds")
var pairRejectChan = make(chan bool, 1)
var templateFile = flag.String("template", "", "Template file to be used to render the messages. Syntax is golang text templating. Column index to be used as placeholder")
var mobField = flag.Int("m", 0, "Mobile Number Field")
var msgField = flag.Int("msg", 1, "If template arg is not specified, use this column number from the CSV as the message to be sent for each user")
var ignoreRows = flag.Int("ignore", 1, "Number of rows to ignore")
var till = flag.Int("till", -1, "Stop processing after <till> number of rows")
var wv = flag.Bool("wv", false, "Print whatsapp version update and exit")
var img = flag.Bool("img", false, "Indicates that media type is image")
var vid = flag.Bool("vid", false, "Indicates that media type is video")
var sepMsg = flag.Bool("sep", false, "Send text as Seprate message when being sent along with media")
var blackList = flag.String("blacklist", "", "File containing blacklisted numbers, one per line")
var msgIdPath = flag.String("msg-ids", "msg_ids.csv", "Location to store jid, message id pairs for later deletion")
var revoke = flag.Int("revoke", -1, "field id of message Id in input to be revoked. This deletes the messages.")
var keepRunning = flag.Int("w", 3600, "Keep running for specified number of seconds to respond to retry messages")

var SLEEP_JITTER = 1000

type inputRow struct {
	V []string
}

type sendChanPayload struct {
	r            string
	sendResponse whatsmeow.SendResponse
}

var saveIds chan sendChanPayload

func main() {

	workerWaiter := sync.WaitGroup{}
	waBinary.IndentXML = true
	flag.Parse()

	if *debugLogs {
		logLevel = "DEBUG"
	}
	if *requestFullSync {
		store.DeviceProps.RequireFullSync = proto.Bool(true)
	}
	log = waLog.Stdout("Main", logLevel, true)
	saveIds = make(chan sendChanPayload)

	dbLog := waLog.Stdout("Database", logLevel, true)
	db := "file:" + *dbAddress + "?_foreign_keys=on"
	storeContainer, err := sqlstore.New(*dbDialect, db, dbLog)
	if err != nil {
		log.Errorf("Failed to connect to database: %v", err)
		return
	}

	latestVer, err := whatsmeow.GetLatestVersion(nil)
	if err != nil {
		log.Infof("Outdated Version", err)
		return
	}
	store.SetWAVersion(*latestVer)
	device, err := storeContainer.GetFirstDevice()
	if err != nil {
		log.Errorf("Failed to get device: %v", err)
		return
	}

	cli = whatsmeow.NewClient(device, waLog.Stdout("Client", logLevel, true))
	var isWaitingForPair atomic.Bool
	cli.PrePairCallback = func(jid types.JID, platform, businessName string) bool {
		isWaitingForPair.Store(true)
		defer isWaitingForPair.Store(false)
		log.Infof("Pairing %s (platform: %q, business name: %q). Type r within 3 seconds to reject pair", jid, platform, businessName)
		select {
		case reject := <-pairRejectChan:
			if reject {
				log.Infof("Rejecting pair")
				return false
			}
		case <-time.After(3 * time.Second):
		}
		log.Infof("Accepting pair")
		return true
	}

	ch, err := cli.GetQRChannel(context.Background())
	if err != nil {
		// This error means that we're already logged in, so ignore it.
		if !errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
			log.Errorf("Failed to get QR channel: %v", err)
		}
	} else {
		go func() {
			for evt := range ch {
				if evt.Event == "code" {
					qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				} else {
					log.Infof("QR channel result: %s", evt.Event)
				}
			}
		}()
	}

	err = cli.Connect()
	if err != nil {
		log.Errorf("Failed to connect: %v", err)
		return
	}
	defer cli.Disconnect()
	var f *os.File

	if *messageFile != "" {
		f, err = os.Open(*messageFile)
	} else {
		f = os.Stdin
	}

	if err != nil {
		log.Errorf("Unable to open message file", err)
		os.Exit(1)
	}
	defer f.Close()

	blackListedNumber := make(map[string]struct{})

	u, _ := user.Current()
	DEFAULT_BL := filepath.Join(u.HomeDir, ".config", "wacsv", "blacklist.csv")
	if *blackList == "" {
		*blackList = DEFAULT_BL
	}

	var blFile *os.File
	blFile, err = os.Open(*blackList)

	if err != nil && *blackList != DEFAULT_BL {
		panic("Unable to read black list file")
	} else {

		scanner := bufio.NewScanner(blFile)

		for scanner.Scan() {
			blackListedNumber[strings.TrimSpace(scanner.Text())] = struct{}{}
		}
		blFile.Close()
	}

	log.Infof("Blacklist length %d", len(blackListedNumber))
	csvReader := csv.NewReader(f)
	sendDoc := *mediaPath != ""

	var uploaded whatsmeow.UploadResponse
	var uploadedThumb whatsmeow.UploadResponse
	var data []byte
	var thumb bytes.Buffer
	var mime string
	var dl int

	type SubImager interface {
		SubImage(r image.Rectangle) image.Image
	}

	imwriter := bufio.NewWriter(&thumb)

	if *msgIdPath != "" {
		go func() {
			workerWaiter.Add(1)
			defer workerWaiter.Done()
			f, err := os.OpenFile(*msgIdPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
			if err != nil {
				log.Errorf("Failed to read %s: %v", *msgIdPath, err)
				os.Exit(1)
			}
			defer f.Close()
			for i := range saveIds {
				_, err := f.WriteString(fmt.Sprintf("%s,%s,%s\n",
					i.r,
					i.sendResponse.ID,
					i.sendResponse.Timestamp))
				if err != nil {

					log.Errorf("%s", err)
				}

			}
		}()
	}

	var height uint32
	var width uint32
	if *mediaPath != "" {
		data, err = os.ReadFile(*mediaPath)

		if err != nil {
			log.Errorf("Failed to read %s: %v", *mediaPath, err)
			return
		}
		if *img {
			i, _, _ := image.Decode(bytes.NewReader(data))
			// check err

			newImage := resize.Resize(72, 0, i, resize.Lanczos3)
			height = 0 // uint32(newImage.Bounds().Dx())
			width = 0  //uint32(newImage.Bounds().Dy())

			// Encode uses a Writer, use a Buffer if you need the raw []byte
			_ = jpeg.Encode(imwriter, newImage, nil)
		} else if *thumbPath != "" {

			data, err := os.ReadFile(*thumbPath)

			if err != nil {
				log.Errorf("Failed to read %s: %v", *thumbPath, err)
				return
			}
			i, _, _ := image.Decode(bytes.NewReader(data))
			// check err

			newImage := resize.Resize(72, 0, i, resize.Lanczos3)

			height = uint32(newImage.Bounds().Dx())
			width = uint32(newImage.Bounds().Dy())
			// Encode uses a Writer, use a Buffer if you need the raw []byte
			_ = jpeg.Encode(imwriter, newImage, nil)

		}
		mime = http.DetectContentType(data)
		dl = len(data)
	}
	if sendDoc {
		if *img {
			uploaded, err = cli.Upload(context.Background(), data, whatsmeow.MediaImage)
		} else if *vid {

			uploaded, err = cli.Upload(context.Background(), data, whatsmeow.MediaVideo)
		} else {

			uploaded, err = cli.Upload(context.Background(), data, whatsmeow.MediaDocument)
		}
		if err != nil {
			log.Errorf("Failed to upload file: %v", err)
		}
	}
	if *img || *thumbPath != "" {

		uploadedThumb, err = cli.Upload(context.Background(), thumb.Bytes(), whatsmeow.MediaImage)
		if err != nil {
			log.Errorf("Failed to upload file: %v", err)
		}
	}
	if *fn == "" && sendDoc {
		*fn = filepath.Base(*mediaPath)
	}
	re := regexp.MustCompile(`{{\s*(\d+)\s*}}`)
	var t *template.Template
	if *templateFile != "" {
		dat, err := os.ReadFile(*templateFile)
		if err != nil {
			panic("Error reading template file")
		}
		messageTemplate := re.ReplaceAllString(string(dat[:]), "{{ index .V ${1} }}")
		fmt.Println(messageTemplate)
		t = template.Must(template.New("").Parse(messageTemplate))
	}

	for r_count := 1; ; r_count++ {
		rec, err := csvReader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Errorf("Error in reading message file %v", err)
			os.Exit(1)
		}
		if r_count <= *ignoreRows {
			continue
		}
		if *till > 1 && (r_count > *till) {
			log.Infof("Exiting as requested r_count %d, till %d", r_count, *till)
			break
		}
		// do something with read line

		message := ""
		rec[*mobField] = strings.TrimSpace(rec[*mobField])

		_, ok := blackListedNumber[rec[*mobField]]
		if ok {
			log.Infof("skipping blacklisted target %s", rec[*mobField])
			continue
		}

		if len(rec[*mobField]) == 10 {
			rec[*mobField] = "+91" + rec[*mobField]
		} else if len(rec[*mobField]) < 10 {
			continue
		}

		if !strings.Contains(rec[*mobField], "@") {
			res, err := cli.IsOnWhatsApp([]string{rec[*mobField]})
			if err != nil {
				log.Errorf("Err getting WA availability for %s", rec[*mobField])
			}
			if !res[0].IsIn {
				log.Errorf("%s not on Whasapp, skipping", rec[*mobField])
				continue
			}

		}

		log.Infof("Row number %d", r_count)
		var tpl bytes.Buffer
		if t != nil {
			payload := inputRow{
				V: rec,
			}
			if err := t.Execute(&tpl, payload); err != nil {
				panic(err)
			}
			message = tpl.String()
		} else {
			message = rec[*msgField]
		}

		// onWA, err := cli.IsOnWhatsApp([]string{rec[*mobField]})
		// if len(onWA) < 1 {

		// 	fmt.Printf("Unable to check for user validity")
		// }
		// if onWA[0].VerifiedName == nil || err != nil {
		// 	fmt.Printf("Invalid User %s", rec[*mobField])
		// 	continue
		// }
		fmt.Printf("%s; %s\n", rec[*mobField], strings.Split(message, "\n")[0])

		if *revoke >= 0 {
			recipient, ok := parseJID(rec[*mobField])
			if !ok {
				continue
			}
			r, err := cli.SendMessage(context.Background(), recipient, cli.BuildRevoke(recipient, *device.ID, rec[*revoke]))
			log.Infof("%s \n %s", r, err)

		} else if !sendDoc {
			log.Infof("Sending Message to %s\n", rec[*mobField])
			sendMessage(rec[*mobField], message)
		} else {
			log.Infof("Sending Message to %s", rec[*mobField])
			m := message
			if *sepMsg {
				message = ""
			}

			if *img {
				sendImage(rec[*mobField], &uploaded, &uploadedThumb, message, &mime, &dl, &thumb, height, width)
			} else if *vid {
				sendVideo(rec[*mobField], &uploaded, message, &mime, &dl, &uploadedThumb, &thumb)
			} else {
				sendDocument(rec[*mobField], &uploaded, fn, message, &mime, &dl, &uploadedThumb, &thumb)
			}
			if *sepMsg {
				sendMessage(rec[*mobField], m)
			}
		}
		v := rand.Intn(SLEEP_JITTER)
		time.Sleep(time.Millisecond * time.Duration((*sleepDuration + v)))

	}

	close(saveIds)
	defer cli.Disconnect()
	workerWaiter.Wait()

	time.Sleep(time.Second * time.Duration(*keepRunning))
	if *keepRunning == 0 {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		<-c
	}

}

func parseJID(arg string) (types.JID, bool) {
	if arg[0] == '+' {
		arg = arg[1:]
	}
	if !strings.ContainsRune(arg, '@') {
		return types.NewJID(arg, types.DefaultUserServer), true
	} else {
		recipient, err := types.ParseJID(arg)
		if err != nil {
			log.Errorf("Invalid JID %s: %v", arg, err)
			return recipient, false
		} else if recipient.User == "" {
			log.Errorf("Invalid JID %s: no server specified", arg)
			return recipient, false
		}
		return recipient, true
	}
}

func sendMessage(jid string, message string) {

	recipient, ok := parseJID(jid)
	if !ok {
		log.Errorf("Invalid Recipient %s", jid)
		return
	}
	msg := &waProto.Message{Conversation: proto.String(message)}
	resp, err := cli.SendMessage(context.Background(), recipient, msg)
	if err != nil {
		log.Errorf("Error sending message: %v receipient: %s", err, jid)
	} else {
		saveIds <- sendChanPayload{jid, resp}
		log.Infof("Message sent (server timestamp: %s)", resp.Timestamp)
	}
}

func sendImage(jid string, uploaded *whatsmeow.UploadResponse, t_u *whatsmeow.UploadResponse, caption string, mime *string, dl *int, thumb *bytes.Buffer, height uint32, width uint32) {
	recipient, ok := parseJID(jid)
	if !ok {
		return
	}
	msg := &waProto.Message{ImageMessage: &waProto.ImageMessage{
		Caption:             proto.String(caption),
		URL:                 proto.String(uploaded.URL),
		DirectPath:          proto.String(uploaded.DirectPath),
		MediaKey:            uploaded.MediaKey,
		Mimetype:            proto.String(*mime),
		FileEncSHA256:       uploaded.FileEncSHA256,
		FileSHA256:          uploaded.FileSHA256,
		FileLength:          proto.Uint64(uint64(*dl)),
		ThumbnailDirectPath: proto.String(t_u.DirectPath),
		ThumbnailSHA256:     t_u.FileSHA256,
		ThumbnailEncSHA256:  t_u.FileEncSHA256,
		JPEGThumbnail:       thumb.Bytes(),
		Height:              &height,
		Width:               &width,
	}}
	resp, err := cli.SendMessage(context.Background(), recipient, msg)
	if err != nil {
		log.Errorf("Error sending document message to %s: %v", jid, err)
	} else {
		saveIds <- sendChanPayload{jid, resp}
		log.Infof("Document message sent (server timestamp: %s)", resp.Timestamp)
	}
}

func sendVideo(jid string, uploaded *whatsmeow.UploadResponse, caption string, mime *string, dl *int, t_u *whatsmeow.UploadResponse, thumb *bytes.Buffer) {
	recipient, ok := parseJID(jid)
	if !ok {
		return
	}
	msg := &waProto.Message{
		VideoMessage: &waProto.VideoMessage{
			Caption:             proto.String(caption),
			URL:                 proto.String(uploaded.URL),
			DirectPath:          proto.String(uploaded.DirectPath),
			MediaKey:            uploaded.MediaKey,
			Mimetype:            proto.String(*mime),
			FileEncSHA256:       uploaded.FileEncSHA256,
			FileSHA256:          uploaded.FileSHA256,
			FileLength:          proto.Uint64(uint64(*dl)),
			ThumbnailDirectPath: proto.String(t_u.DirectPath),
			ThumbnailSHA256:     t_u.FileSHA256,
			ThumbnailEncSHA256:  t_u.FileEncSHA256,
			JPEGThumbnail:       thumb.Bytes(),
		}}
	resp, err := cli.SendMessage(context.Background(), recipient, msg)
	if err != nil {
		log.Errorf("Error sending video message to %s: %v", jid, err)
	} else {
		log.Infof("Video message sent (server timestamp: %s)", resp.Timestamp)
	}
}

func sendDocument(jid string, uploaded *whatsmeow.UploadResponse, fn *string, caption string, mime *string, dl *int, t_u *whatsmeow.UploadResponse, thumb *bytes.Buffer) {
	recipient, ok := parseJID(jid)
	if !ok {
		return
	}
	msg := &waProto.Message{
		DocumentMessage: &waProto.DocumentMessage{
			Caption:             proto.String(caption),
			URL:                 proto.String(uploaded.URL),
			DirectPath:          proto.String(uploaded.DirectPath),
			MediaKey:            uploaded.MediaKey,
			Mimetype:            proto.String(*mime),
			FileEncSHA256:       uploaded.FileEncSHA256,
			FileSHA256:          uploaded.FileSHA256,
			FileLength:          proto.Uint64(uint64(*dl)),
			FileName:            fn,
			ThumbnailDirectPath: proto.String(t_u.DirectPath),
			ThumbnailSHA256:     t_u.FileSHA256,
			ThumbnailEncSHA256:  t_u.FileEncSHA256,
			JPEGThumbnail:       thumb.Bytes(),
		}}
	resp, err := cli.SendMessage(context.Background(), recipient, msg)
	if err != nil {
		log.Errorf("Error sending document message to %s: %v", jid, err)
	} else {
		log.Infof("Document message sent (server timestamp: %s)", resp.Timestamp)
	}
}
