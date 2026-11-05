// Package notify tells a house owner by email that a guest was recorded at the gate (PRD S3).
//
// Check-in writes a row to notification_outbox in its own transaction; a Worker sends the emails
// afterwards, so a slow or broken mail server never slows the guard down. The message is a fixed
// template that names no guest: only the category (a value from a fixed list) and the arrival time.
package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
)

// Message is one email.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Sender delivers a Message. SMTP is the only implementation today; a WhatsApp or push sender can
// replace it without touching the worker or check-in.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// categoryLabel is how each visit category reads in the message. Anything unknown reads as "tamu",
// so a category can never smuggle free text into an email.
var categoryLabel = map[string]string{
	"tamu":     "tamu",
	"kurir":    "kurir",
	"ojol":     "ojol",
	"tukang":   "tukang",
	"keluarga": "keluarga",
	"lainnya":  "tamu lain",
}

// arrivalMessage is the "guest arrived" email. The only inputs are the owner's address, the visit
// category and the arrival time; no guest name, plate or photo ever reaches the mail server.
func arrivalMessage(to, category string, at time.Time) Message {
	label, ok := categoryLabel[category]
	if !ok {
		label = "tamu"
	}
	return Message{
		To:      to,
		Subject: "Tamu tiba di rumah Anda",
		Body: fmt.Sprintf("Ada tamu (%s) tiba di rumah Anda pukul %s WIB.\n\n"+
			"Buka aplikasi Buku Tamu untuk menyetujui atau menolak.\n", label, at.In(httpx.WIB).Format("15:04")),
	}
}
