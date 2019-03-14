package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nlopes/slack"
	"github.com/pkg/errors"
)

type server struct {
	router      *http.ServeMux
	port        string
	logger      *log.Logger
	slackClient *slack.Client
	botID       string
	token       string
}

// coffeeOrders records order
var coffeeOrders = make(map[string]map[string]string)

func (s *server) routes() {
	s.router.HandleFunc("/slack/message_actions", s.messageActionHandler())
}

var actionOrder = "coffee_order"

func (s *server) listenAndResponse() {
	rtm := s.slackClient.NewRTM()

	// Start listening slack events
	go rtm.ManageConnection()

	// Handle slack events
	for msg := range rtm.IncomingEvents {
		switch ev := msg.Data.(type) {
		case *slack.MessageEvent:
			if err := s.handleMessageEvent(ev); err != nil {
				log.Printf("[ERROR] Failed to handle message: %s", err)
			}
		}
	}
}

func (s *server) handleMessageEvent(ev *slack.MessageEvent) error {
	// Only response mention to bot. Ignore else.
	if !strings.HasPrefix(ev.Msg.Text, fmt.Sprintf("<@%s> ", s.botID)) {
		log.Print(ev.Msg.Text)
		log.Printf("%s %s", ev.Channel, fmt.Sprintf("<@%s> ", s.botID))
		return nil
	}

	// Parse message
	m := strings.Split(strings.TrimSpace(ev.Msg.Text), " ")[1:]
	if len(m) == 0 || m[0] != "order" {
		log.Printf("%s %s", ev.Channel, m[0])
		return nil
	}

	// value is passed to message handler when request is approved.
	attachment := slack.Attachment{
		Text:       "I am Coffeebot :robot_face:, and I'm here to help bring you fresh coffee :coffee:",
		Color:      "#3AA3E3",
		CallbackID: s.botID + "coffee_order_form",
		Actions: []slack.AttachmentAction{
			{
				Name:  actionOrder,
				Text:  ":coffee: Order Coffee",
				Type:  "button",
				Value: actionOrder,
			},
		},
	}

	options := slack.MsgOptionAttachments(attachment)

	if _, _, err := s.slackClient.PostMessage(ev.Channel, options); err != nil {
		return fmt.Errorf("failed to post message: %s", err)
	}

	return nil
}

func (s *server) messageActionHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			log.Print("invalid method: ", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		buf, err := ioutil.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			log.Print("read request body failed: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		jsonStr, err := url.QueryUnescape(string(buf)[8:])
		if err != nil {
			log.Printf("[ERROR] Failed to unespace request body: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// var message slack.AttachmentActionCallback // DEPRECATED
		var message slack.InteractionCallback
		if err := json.Unmarshal([]byte(jsonStr), &message); err != nil {
			log.Print("json unmarshal message failed: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if message.Token != s.token {
			log.Print("token invalid")
			w.WriteHeader(http.StatusUnauthorized)
		}

		if message.Type == slack.InteractionTypeInteractionMessage {
			if _, ok := coffeeOrders[message.User.ID]["MessageTs"]; !ok {
				coffeeOrders[message.User.ID] = make(map[string]string)
			}
			coffeeOrders[message.User.ID]["MessageTs"] = message.MessageTs

			dialog := makeDialog(message.User.ID)
			if err := s.slackClient.OpenDialogContext(context.TODO(), message.TriggerID, *dialog); err != nil {
				log.Print("open dialog failed: ", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			originalMessage := message.OriginalMessage
			originalMessage.ReplaceOriginal = true
			originalMessage.Timestamp = coffeeOrders[message.User.ID]["order_channel"]
			originalMessage.Text = ":pencil: Taking your order..."
			originalMessage.Attachments = []slack.Attachment{}
			w.Header().Add("Content-type", "application/json")
			json.NewEncoder(w).Encode(&originalMessage)
			return

		} else if message.Type == slack.InteractionTypeDialogSubmission {
			t := message.Submission["timeToDeliver"]
			if err := validateTime(t); err != nil {
				log.Print("validate error: ", err)

				w.Header().Add("Content-type", "application/json")
				json.NewEncoder(w).Encode(&slack.DialogInputValidationErrors{
					[]slack.DialogInputValidationError{{Name: "timeToDeliver", Error: err.Error()}},
				})
				return
			}

			go func() {
				time.Sleep(time.Second * 5)

				attachment := slack.Attachment{
					Text:       ":white_check_mark: Order received!",
					CallbackID: s.botID + "coffee_order_form",
				}
				options := slack.MsgOptionAttachments(attachment)
				if _, _, err := s.slackClient.PostMessage(message.Channel.ID, options); err != nil {
					log.Print("[ERROR] Failed to post message")
				}
				return
			}()

			w.Header().Add("Content-type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "")
		}

		return
	}
}

var errInvalidInput = errors.New("invalid input")
var errTimePreference = errors.New("time should be after 30 minutes ago")

func validateTime(t string) error {
	const format = "15:04"
	parsedTime, err := time.Parse(format, t)
	if err != nil {
		return errInvalidInput
	}

	var jst = time.FixedZone("UTC+9", 9*60*60)
	now := time.Now().In(jst)

	addLocationJST := func(t time.Time) time.Time {
		return time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), jst)
	}
	parsedTime = addLocationJST(parsedTime)

	if parsedTime.Before(now.Add(time.Minute * 30)) {
		return errTimePreference
	}

	return nil
}

func makeDialog(userID string) *slack.Dialog {
	return &slack.Dialog{
		Title:       "Request a coffee",
		SubmitLabel: "Submit",
		CallbackID:  userID + "coffee_order_form",
		Elements: []slack.DialogElement{
			slack.DialogInputSelect{
				DialogInput: slack.DialogInput{
					Label:       "Coffee Type",
					Type:        slack.InputTypeSelect,
					Name:        "mealPreferences",
					Placeholder: "Select a drink",
				},
				Options: []slack.DialogSelectOption{
					{
						Label: "Cappuccino",
						Value: "cappuccino",
					},
					{
						Label: "Latte",
						Value: "latte",
					},
					{
						Label: "Pour Over",
						Value: "pourOver",
					},
					{
						Label: "Cold Brew",
						Value: "coldBrew",
					},
				},
			},
			slack.DialogInput{
				Label:    "Customization orders",
				Type:     slack.InputTypeTextArea,
				Name:     "customizePreference",
				Optional: true,
			},
			slack.DialogInput{
				Label:       "Time to deliver",
				Type:        slack.InputTypeText,
				Name:        "timeToDeliver",
				Placeholder: "hh:mm",
			},
		},
	}
}

func main() {
	app := server{
		router:      http.NewServeMux(),
		port:        "3000",
		logger:      log.New(os.Stdout, "", log.Lshortfile),
		slackClient: slack.New(os.Getenv("BOT_TOKEN")),
		token:       os.Getenv("VERIFICATION_TOKEN"),
		botID:       os.Getenv("BOT_ID"),
	}

	go app.listenAndResponse()

	app.routes()
	log.Fatal(http.ListenAndServe(":"+app.port, app.router))
}
