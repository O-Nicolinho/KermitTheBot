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
			return // only care about slash/interactions
		}

		switch ic.ApplicationCommandData().Name {

		// ─────────────────────────  /remind  ──────────────────────────
		case "remind":
			// pull the 3 option values
			var (
				timeStr string
				tzStr   string
				msgStr  string
			)
			for _, opt := range ic.ApplicationCommandData().Options {
				switch opt.Name {
				case "time":
					timeStr = opt.StringValue() // "06:35"
				case "timezone":
					tzStr = opt.StringValue() // "America/Toronto"
				case "message":
					msgStr = opt.StringValue() // "uwu"
				}
			}
			if timeStr == "" || tzStr == "" || msgStr == "" {
				respond(s, ic, "All three options (time, timezone, message) are required.")
				return
			}

			// validate HH:MM
			parts := strings.Split(timeStr, ":")
			if len(parts) != 2 {
				respond(s, ic, "Time must be HH:MM (24‑hour).")
				return
			}
			hour, min := atoi(parts[0]), atoi(parts[1])
			if hour < 0 || hour > 23 || min < 0 || min > 59 {
				respond(s, ic, "Time must be a valid 24‑hour clock value.")
				return
			}

			// validate timezone
			loc, err := time.LoadLocation(tzStr)
			if err != nil {
				respond(s, ic, "Invalid timezone name.")
				return
			}

			// insert into Postgres
			row := Reminder{
				UserID:    ic.Member.User.ID,
				ChannelID: ic.ChannelID,
				Message:   msgStr,
				Hour:      hour,
				Min:       min,
				TZ:        tzStr,
				Active:    true,
			}

			err = db.QueryRow(
				context.Background(),
				`INSERT INTO reminders(user_id,channel_id,message,hour,minute,tz,active)
				 VALUES ($1,$2,$3,$4,$5,$6,true)
				 RETURNING id`,
				row.UserID, row.ChannelID, row.Message, row.Hour, row.Min, row.TZ,
			).Scan(&row.ID)
			if err != nil {
				respond(s, ic, "Database error while saving your reminder.")
				return
			}

			// schedule the daily reminder
			row.CronID = scheduleOne(sched, row, s, loc)

			respond(
				s, ic,
				fmt.Sprintf("Got it! I’ll remind you every day at %02d:%02d %s (ID %d)",
					hour, min, tzStr, row.ID),
			)

		// ───────────────────────────  /stop  ──────────────────────────
		case "stop":

			if len(ic.ApplicationCommandData().Options) == 0 {
				respond(s, ic, "Usage: /stop <reminder‑ID>")
				return
			}
			id := ic.ApplicationCommandData().Options[0].IntValue()

			/* mark inactive in DB */
			if _, err := db.Exec(
				context.Background(),
				`UPDATE reminders SET active=false WHERE id=$1`,
				id,
			); err != nil {
				respond(s, ic, "Database error while stopping reminder.")
				return
			}

			respond(s, ic, fmt.Sprintf("Reminder %d stopped ✅", id))
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

func scheduleOne(_ *cron.Cron, r Reminder, s *discordgo.Session, loc *time.Location) cron.EntryID {
	c := cron.New(cron.WithLocation(loc))

	spec := fmt.Sprintf("%d %d * * *", r.Min, r.Hour)

	id, _ := c.AddFunc(spec, func() {

		if s != nil {
			s.ChannelMessageSend(r.ChannelID, "<@"+r.UserID+"> "+r.Message)
		}
	})

	c.Start()

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
