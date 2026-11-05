package report

import (
	"bytes"
	"fmt"
	"sort"
	"time"

	"github.com/go-pdf/fpdf"
)

// Page geometry in millimetres: A4 landscape with 10 mm side margins leaves 277 mm for the table.
const (
	pageMargin   = 10.0
	bottomMargin = 15.0
	rowHeight    = 5.5
)

// Column widths, summing to 277 mm with personal data and to 178 mm without (the table then
// simply ends before the right margin).
var (
	baseWidths     = []float64{30, 30, 14, 30, 20, 18, 22, 14}
	personalWidths = []float64{45, 24, 30}
)

// buildPDF renders the report. It is built entirely in memory so a failure is known before the first
// byte is sent. Compression is off: a report is small text, and an uncompressed file can be inspected
// and diffed. Only core fonts are used, so characters outside Latin-1 are shown as "?".
func buildPDF(rows []Row, p Params, generated time.Time) ([]byte, error) {
	pdf := fpdf.New("L", "mm", "A4", "")
	pdf.SetCompression(false)
	pdf.SetTitle("Laporan Kunjungan Buku Tamu", true)
	pdf.SetMargins(pageMargin, pageMargin, pageMargin)
	pdf.SetAutoPageBreak(true, bottomMargin)
	pdf.AliasNbPages("")
	tr := pdf.UnicodeTranslatorFromDescriptor("")

	pdf.SetFooterFunc(func() {
		pdf.SetY(-10)
		pdf.SetFont("Helvetica", "", 8)
		pdf.CellFormat(0, 5, tr(fmt.Sprintf("Halaman %d dari {nb}", pdf.PageNo())), "", 0, "C", false, 0, "")
	})

	widths := append([]float64{}, baseWidths...)
	if p.IncludePersonal {
		widths = append(widths, personalWidths...)
	}
	heads := columns(p.IncludePersonal)
	_, pageH := pdf.GetPageSize()

	header := func() {
		pdf.SetFont("Helvetica", "B", 8)
		pdf.SetFillColor(225, 225, 225)
		for i, h := range heads {
			pdf.CellFormat(widths[i], rowHeight, fit(pdf, tr(h), widths[i]), "1", 0, "L", true, 0, "")
		}
		pdf.Ln(-1)
		pdf.SetFont("Helvetica", "", 8)
	}

	pdf.AddPage()
	pdf.SetFont("Helvetica", "B", 15)
	pdf.CellFormat(0, 8, tr("Laporan Kunjungan Buku Tamu"), "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 9)
	pdf.CellFormat(0, 5, tr(fmt.Sprintf("Periode: %s sampai %s (WIB)", p.From.Format(time.DateOnly), p.To.Format(time.DateOnly))), "", 1, "L", false, 0, "")
	pdf.CellFormat(0, 5, tr("Dibuat: "+generated.Format("2006-01-02 15:04")+" WIB"), "", 1, "L", false, 0, "")
	if scope := scopeLine(p); scope != "" {
		pdf.CellFormat(0, 5, tr(scope), "", 1, "L", false, 0, "")
	}
	if p.IncludePersonal {
		pdf.CellFormat(0, 5, tr("Laporan ini memuat data pribadi tamu. Simpan dan bagikan dengan hati-hati."), "", 1, "L", false, 0, "")
	}
	pdf.Ln(2)

	// Summary.
	total, inside, over := len(rows), 0, 0
	byCategory := map[string]int{}
	for _, r := range rows {
		byCategory[r.Category]++
		if r.Status == "inside" {
			inside++
		}
		if r.Overstay {
			over++
		}
	}
	pdf.SetFont("Helvetica", "B", 10)
	pdf.CellFormat(0, 6, tr(fmt.Sprintf("Total kunjungan: %d", total)), "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 9)
	cats := make([]string, 0, len(byCategory))
	for c := range byCategory {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	for _, c := range cats {
		pdf.CellFormat(0, 5, tr(fmt.Sprintf("  %s: %d", c, byCategory[c])), "", 1, "L", false, 0, "")
	}
	pdf.CellFormat(0, 5, tr(fmt.Sprintf("Masih di dalam: %d", inside)), "", 1, "L", false, 0, "")
	pdf.CellFormat(0, 5, tr(fmt.Sprintf("Lewat batas lama tinggal: %d", over)), "", 1, "L", false, 0, "")
	pdf.Ln(3)

	// Table. The header repeats on every page: a row is moved to the next page by hand, before the
	// automatic page break could split it from its header.
	header()
	for _, r := range rows {
		if pdf.GetY()+rowHeight > pageH-bottomMargin {
			pdf.AddPage()
			header()
		}
		for i, c := range cells(r, p.IncludePersonal) {
			pdf.CellFormat(widths[i], rowHeight, fit(pdf, tr(c), widths[i]), "1", 0, "L", false, 0, "")
		}
		pdf.Ln(-1)
	}
	if total == 0 {
		pdf.SetFont("Helvetica", "I", 9)
		pdf.CellFormat(0, 6, tr("Tidak ada kunjungan pada periode ini."), "", 1, "L", false, 0, "")
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("render pdf: %w", err)
	}
	return buf.Bytes(), nil
}

func scopeLine(p Params) string {
	var s string
	add := func(k, v string) {
		if s != "" {
			s += ", "
		}
		s += k + ": " + v
	}
	if p.HouseID != nil {
		add("rumah", p.HouseID.String())
	}
	if p.Category != "" {
		add("kategori", p.Category)
	}
	if p.Status != "" {
		add("status", label(statusLabel, p.Status))
	}
	if s == "" {
		return ""
	}
	return "Filter - " + s
}

// fit shortens s with "..." so it fits a cell of width w. s is already translated to the PDF's
// single-byte encoding, so cutting bytes never splits a character.
func fit(pdf *fpdf.Fpdf, s string, w float64) string {
	const pad = 2.0
	if pdf.GetStringWidth(s) <= w-pad {
		return s
	}
	for len(s) > 0 && pdf.GetStringWidth(s+"...") > w-pad {
		s = s[:len(s)-1]
	}
	return s + "..."
}
