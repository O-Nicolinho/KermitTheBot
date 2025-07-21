package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
	"github.com/robfig/cron/v3"
)

type Reminder struct {
	ID        int
	UserID    string
	ChannelID string
	Message   string
	Hour      int
	Min       int
	TZ        string
	Active    bool
	CronID    cron.EntryID
}

func main() {

	token := mustEnv("DISCORD_TOKEN")

	dsn := mustEnv("DATABASE_URL")

	port := os.Getenv("PORT")

	if port == "" {
		port = "8080"
	}

	// ======= Postgres ========

	db, err := pgx.Connect(context.Background(), dsn)

	if err != nil {
		log.Fatal(err)
	}
	defer db.Close(context.Background())

	// ========        =========
	if _, err := db.Exec(context.Background(), schema); err != nil {
		log.Fatal(err)
	}

	// ======= Scheduler ========

	sched := cron.New()

	restoreJobs(db, sched)
	sched.Start()

	defer sched.Stop()

	// ======= Discord ========

	dg, _ := discordgo.New("Bot " + token)
	dg.AddHandler(onSlash(db, sched))

	if err := dg.Open(); err != nil {
		log.Fatal(err)
	}

	ensureCommands(dg)

	defer dg.Close()

	// ======= Web Server ========

	go func() {
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
		log.Fatal(http.ListenAndServe(":"+port, nil))
	}()

	stop := make(chan os.Signal, 1)

	signal.Notify(stop, os.Interrupt)
	<-stop

}

// ======= Helpers ========

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s environment variable required", k)
	}
	return v
}

func onSlash(db *pgx.Conn, sched *cron.Cron) func(*discordgo.Session, *discordgo.InteractionCreate) {
	return func(s *discordgo.Session, ic *discordgo.InteractionCreate) {
		if ic.Type != discordgo.InteractionApplicationCommand {
			return
		}

		name := ic.ApplicationCommandData().Name

		switch name {

		case "remind": // eg: remind 09:00 America/Toronto water your plants

			args := strings.SplitN(ic.ApplicationCommandData().Options[0].StringValue(), " ", 3)

			if len(args) < 3 {
				respond(s, ic, "Invalid input: correct usage is /remind HH:MM Timezone Mesage")
				return
			}

			tkn := strings.Split(args[0], ":")
			if len(tkn) != 2 {
				respond(s, ic, "Correct time format is HH:MM.")
				return
			}

			hour, min := atoi(tkn[0]), atoi(tkn[1])

			loc, err := time.LoadLocation(args[1])

			if err != nil {
				respond(s, ic, "Invalid timezone")
				return

			}

			msg := args[2]

			row := Reminder{
				UserID:    ic.Member.User.ID,
				ChannelID: ic.ChannelID,
				Message:   msg,
				Hour:      hour,
				Min:       min,
				TZ:        args[1],
				Active:    true,
			}

			err = db.QueryRow(context.Background(),
				`INSERT INTO reminders(user_id, channel_id, message, hour, minute, tz, active)
			 VALUES($1,$2,$3,$4,$5,$6,true)
			 RETURNING ID`,
				row.UserID, row.ChannelID, row.Message, row.Hour, row.Min, row.TZ,
			).Scan(&row.ID)

			if err != nil {
				respond(s, ic, "db error")
				return
			}

			row.CronID = scheduleOne(sched, row, s, loc)

			respond(s, ic, fmt.Sprintf("Reminder Set! I will remind you daily at %02d:%02d %s", hour, min, args[1]))

		case "stop":
			id := ic.ApplicationCommandData().Options[0].IntValue()
			_, err := db.Exec(context.Background(), "UPDATE reminders SET active=false WHERE id=$1", id)
			if err != nil {
				respond(s, ic, "db error")
				return
			}
			respond(s, ic, "Reminder stopped successfully!")
		}

	}
}

func respond(s *discordgo.Session, ic *discordgo.InteractionCreate, msg string) {
	s.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: msg},
	})
}

func restoreJobs(db *pgx.Conn, c *cron.Cron) {
	rows, _ := db.Query(context.Background(),
		`SELECT id,user_id,channel_id,message,hour,minute,tz FROM reminders WHERE active`)
	defer rows.Close()

	for rows.Next() {
		var r Reminder
		if err := rows.Scan(&r.ID, &r.UserID, &r.ChannelID, &r.Message, &r.Hour, &r.Min, &r.TZ); err != nil {
			continue
		}
		loc, err := time.LoadLocation(r.TZ)
		if err != nil {
			continue
		}
		scheduleOne(c, r, nil, loc) // Discord session is nil because we can't send messages during cold start
	}
}

func scheduleOne(c *cron.Cron, r Reminder, s *discordgo.Session, loc *time.Location) cron.EntryID {
	spec := fmt.Sprintf("%d %d * * *", r.Min, r.Hour) // m h dom mon dow
	id, _ := c.AddFunc(spec, func() {
		s.ChannelMessageSend(r.ChannelID, "<@"+r.UserID+"> "+r.Message)
	})
	return id
}

func atoi(s string) int {
	i, _ := strconv.Atoi(s)
	return i
}

func ensureCommands(dg *discordgo.Session) {
	appID := dg.State.User.ID
	cmds, _ := dg.ApplicationCommands(appID, "")
	if len(cmds) > 0 {
		return
	} // already registered

	_, _ = dg.ApplicationCommandCreate(appID, "", &discordgo.ApplicationCommand{
		Name: "remind", Description: "Create a daily reminder",
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionString, Name: "time", Description: "HH:MM", Required: true},
			{Type: discordgo.ApplicationCommandOptionString, Name: "timezone", Description: "TZ name", Required: true},
			{Type: discordgo.ApplicationCommandOptionString, Name: "message", Description: "Text", Required: true},
		},
	})
	_, _ = dg.ApplicationCommandCreate(appID, "", &discordgo.ApplicationCommand{
		Name: "stop", Description: "Cancel a reminder",
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionInteger, Name: "id", Description: "Reminder ID", Required: true},
		},
	})
}

const schema = `
CREATE TABLE IF NOT EXISTS reminders (
	id          SERIAL PRIMARY KEY,
	user_id     TEXT,
	channel_id  TEXT,
	message     TEXT,
	hour        INT,
	minute      INT,
	tz          TEXT,
	active      BOOLEAN DEFAULT TRUE
);`
