package report

import (
	"encoding/csv"
	"io"
	"strconv"
)

// utf8BOM makes Excel read the file as UTF-8 instead of the local code page.
const utf8BOM = "\xEF\xBB\xBF"

// columns returns the report's column headers, in Indonesian like the rest of the user-facing text.
func columns(personal bool) []string {
	cols := []string{"Waktu masuk (WIB)", "Waktu keluar (WIB)", "Lama (menit)", "Rumah", "Kategori", "Status", "Keputusan pemilik", "Lewat batas"}
	if personal {
		cols = append(cols, "Nama tamu", "Plat nomor", "Keterangan")
	}
	return cols
}

var (
	statusLabel   = map[string]string{"inside": "di dalam", "exited": "keluar"}
	decisionLabel = map[string]string{"pending": "menunggu", "approved": "disetujui", "rejected": "ditolak"}
)

func label(m map[string]string, k string) string {
	if v, ok := m[k]; ok {
		return v
	}
	return k
}

// cells renders one row as text, the same for the CSV and the PDF.
func cells(r Row, personal bool) []string {
	exit, minutes := "", ""
	if r.CheckoutAt != nil {
		exit = r.CheckoutAt.Format("2006-01-02 15:04")
	}
	if r.MinutesInside != nil {
		minutes = strconv.Itoa(*r.MinutesInside)
	}
	over := "tidak"
	if r.Overstay {
		over = "ya"
	}
	c := []string{
		r.CheckinAt.Format("2006-01-02 15:04"), exit, minutes, r.House, r.Category,
		label(statusLabel, r.Status), label(decisionLabel, r.OwnerDecision), over,
	}
	if personal {
		c = append(c, r.GuestName, r.Plate, r.Note)
	}
	return c
}

// safeCell stops a spreadsheet from running a cell as a formula (CSV injection). A guest could give
// the name "=HYPERLINK(...)" and the admin opening the export would run it. A cell that starts with
// = + - @ or a tab or carriage return gets a leading apostrophe, which Excel and Sheets show as text.
func safeCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// csvWriter writes the report as CSV with a byte-order mark, page by page.
type csvWriter struct {
	w        *csv.Writer
	personal bool
}

func newCSVWriter(w io.Writer, personal bool) (*csvWriter, error) {
	if _, err := io.WriteString(w, utf8BOM); err != nil {
		return nil, err
	}
	c := &csvWriter{w: csv.NewWriter(w), personal: personal}
	if err := c.w.Write(columns(personal)); err != nil {
		return nil, err
	}
	// Flushed now, not with the first page: a period with no visits must still send its header.
	c.w.Flush()
	if err := c.w.Error(); err != nil {
		return nil, err
	}
	return c, nil
}

// writePage writes the rows and flushes them to the client.
func (c *csvWriter) writePage(page []Row) error {
	for _, r := range page {
		cs := cells(r, c.personal)
		for i := range cs {
			cs[i] = safeCell(cs[i])
		}
		if err := c.w.Write(cs); err != nil {
			return err
		}
	}
	c.w.Flush()
	return c.w.Error()
}
