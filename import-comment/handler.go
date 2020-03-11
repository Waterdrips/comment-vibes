package function

import (
	"database/sql"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/brankas/emoji"
	"github.com/google/go-github/github"
	_ "github.com/lib/pq"
	"github.com/pkg/errors"

	"github.com/openfaas/openfaas-cloud/sdk"
)

var db *sql.DB

// init establishes a persistent connection to the remote database
// the function will panic if it cannot establish a link and the
// container will restart / go into a crash/back-off loop
func init() {
	if _, err := os.Stat("/var/openfaas/secrets/password"); err == nil {
		password, _ := sdk.ReadSecret("password")
		user, _ := sdk.ReadSecret("username")
		host, _ := sdk.ReadSecret("host")

		dbName := os.Getenv("postgres_db")
		port := os.Getenv("postgres_port")
		sslmode := os.Getenv("postgres_sslmode")

		connStr := "postgres://" + user + ":" + password + "@" + host + ":" + port + "/" + dbName + "?sslmode=" + sslmode

		var err error
		db, err = sql.Open("postgres", connStr)

		if err != nil {
			panic(err.Error())
		}

		err = db.Ping()
		if err != nil {
			panic(err.Error())
		}
	}
}

// Handle a HTTP request as a middleware processor.
func Handle(w http.ResponseWriter, r *http.Request) {

	if r.Body != nil {
		defer r.Body.Close()
	}

	dbErr := db.Ping()
	if dbErr != nil {
		w.WriteHeader(http.StatusOK)
		log.Printf("Error performing ping: %s\n", dbErr)

		http.Error(w, dbErr.Error(), http.StatusInternalServerError)
		return
	}

	body, _ := ioutil.ReadAll(r.Body)

	if enforceHMAC, ok := os.LookupEnv("enforce_hmac"); !ok || enforceHMAC == "true" {

		webhookSecret, webhookSecretErr := sdk.ReadSecret("webhook-secret")
		if webhookSecretErr != nil {
			wrappedErr := errors.Wrap(webhookSecretErr, "unable to read secret")
			log.Printf("%s\n", wrappedErr)
			http.Error(w, wrappedErr.Error(), http.StatusBadRequest)
			return
		}

		// Validate using HMAC that the incoming request is signed by GitHub using the
		// symmetric key.
		invalid := github.ValidateSignature(r.Header.Get("X-Hub-Signature"), body, []byte(webhookSecret))
		if invalid != nil {

			wrappedErr := errors.Wrap(invalid, "signature was invalid")
			log.Printf("%s\n", wrappedErr.Error())
			http.Error(w, wrappedErr.Error(), http.StatusBadRequest)

			return
		}
	}

	webhookType := github.WebHookType(r)
	event, err := github.ParseWebHook(webhookType, body)

	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}

	// Only two event types are supported for logging
	msg := ""

	if issueCommentEvent, ok := event.(*github.IssueCommentEvent); ok {
		switch *issueCommentEvent.Action {
		case "created":

			msg = " (comment created) by " + issueCommentEvent.Sender.GetLogin()
			login := issueCommentEvent.Sender.GetLogin()
			id := issueCommentEvent.Sender.GetID()

			insertErr := insertUser(login, id, true)

			if insertErr != nil {
				if !isRowConflict(insertErr.Error()) {
					log.Printf("%s\n", insertErr.Error())
				}
			}

			trimmed := trim(issueCommentEvent.GetComment().GetBody())
			if isEmoji(trimmed) {

				activityErr := insertActivity(id, emoji.ReplaceAliases(trimmed))

				if activityErr != nil {
					if !isRowConflict(activityErr.Error()) {
						log.Printf("%s\n", activityErr.Error())
					}

				}
			} else {
				log.Printf("%q was not an emoji\n", trimmed)
			}

		}
	}

	w.WriteHeader(http.StatusOK)
	// This message will appear on your GitHub webhook audit page
	w.Write([]byte(fmt.Sprintf("Ping OK %s", msg)))
}

// insertUser will insert a user, or fail if the row already exists, this could be
// converted to an "upsert"
func insertUser(login string, ID int64, track bool) error {
	res, err := db.Query(`insert into users (user_id, user_login, track, created_at) values ($1, $2, $3, now());`,
		ID, login, track)

	if err == nil {
		defer res.Close()
	}

	return err
}

// insertActivity tracks the activity using now() for the date/time
func insertActivity(loginID int64, emoji string) error {
	res, err := db.Query(`insert into activity (id, user_id, activity_date, emoji) values (DEFAULT, $1, now(), $2);`,
		loginID, emoji)
	if err == nil {
		defer res.Close()
	}

	return err
}

func isRowConflict(err string) bool {
	return strings.Contains(err, "duplicate key value violates unique constraint")
}

func trim(input string) string {
	val := strings.TrimSpace(input)
	val = emoji.ReplaceCodes(val)

	return val
}

func isEmoji(input string) bool {
	sep := ([]byte(`:`))[0]

	if len(input) >= 3 {
		// No composites
		if strings.Count(input, ":") == 2 {
			return input[0] == sep && input[len(input)-1] == sep
		}
	}

	return false
}
