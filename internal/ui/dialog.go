package ui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/mux"
)

// DialogHost is what the new-window dialog needs of the multiplexer.
type DialogHost interface {
	Config() *config.Config
	Recent() (images, repos []string)
	Pools(ctx context.Context) ([]api.Pool, error)
	Validate(ctx context.Context, name string, p config.Profile) (*api.SubmitAnswer, error)
	// Background is the shown window's screen as plain text, drawn
	// dimmed around the box.
	Background(rows, cols int) []string
}

// stop is a place the focus can be.
type stop int

const (
	stProfile stop = iota
	stImage
	stPool
	stSize
	stCPUs
	stMemory
	stDisk
	stRepo
	stBranch
	stSpare
	stSaveAs
	stCreate
	stCancel
)

// fieldStop is where each of DialogValues' field errors is shown.
var fieldStop = map[string]stop{
	FieldImage: stImage, FieldPool: stPool, FieldSize: stSize, FieldCPUs: stCPUs, FieldMemory: stMemory,
	FieldDisk: stDisk, FieldRepository: stRepo, FieldBranch: stBranch, FieldSaveAs: stSaveAs,
}

const (
	labelWidth   = 19
	validateFor  = 30 * time.Second
	poolsFor     = 15 * time.Second
	dropdownRows = 6
)

type poolsState int

const (
	poolsLoading poolsState = iota
	poolsLoaded
	poolsFailed
)

type poolsMsg struct {
	pools []api.Pool
	err   error
}

type validatedMsg struct {
	gen int
	req *mux.NewWindowRequest
	ans *api.SubmitAnswer
	err error
}

// Dialog is the new-window dialog's model: one form with every field of
// the window's profile, checked with lux's dry run before it is created.
type Dialog struct {
	ctx        context.Context
	host       DialogHost
	cfg        *config.Config
	rows, cols int
	bg         []string

	profile, pool, size *dropdown
	in                  map[stop]*textinput.Model
	spare               bool
	pools               []api.Pool
	poolsState          poolsState

	focus  stop
	scroll int
	// edited are the fields changed in this dialog: a profile change
	// leaves them as they are.
	edited map[stop]bool
	errs   map[stop]string

	status     string
	statusErr  bool
	validating bool
	spin       spinner.Model
	// gen counts edits: a dry run's answer for older values is dropped.
	gen int
	// confirm is the request a dry run found valid but not startable now:
	// a second Create makes it.
	confirm *mux.NewWindowRequest

	// Request is what to create once Done (nil: cancelled).
	Request *mux.NewWindowRequest
	Done    bool
}

// NewDialogModel opens the dialog on the configuration's default profile.
// Its pools load in the background (Init).
func NewDialogModel(ctx context.Context, h DialogHost, rows, cols int) *Dialog {
	cfg := h.Config()
	d := &Dialog{ctx: ctx, host: h, cfg: cfg, rows: rows, cols: cols, edited: map[stop]bool{}, errs: map[stop]string{},
		in: map[stop]*textinput.Model{}, spin: spinner.New(spinner.WithSpinner(spinner.Line))}
	var names []option
	for _, n := range cfg.ProfileNames() {
		names = append(names, option{n, n})
	}
	d.profile = &dropdown{value: cfg.DefaultProfile}
	d.profile.setOptions(names)
	d.pool = &dropdown{}
	d.size = &dropdown{}
	images, repos := h.Recent()
	sugg := map[stop][]string{
		stImage: uniq(append(profileField(cfg, func(p config.Profile) string { return p.Image }), images...)),
		stRepo:  uniq(append(profileField(cfg, func(p config.Profile) string { return p.Repository }), repos...)),
	}
	place := map[stop]string{stBranch: "remote HEAD", stSaveAs: "don't save", stRepo: "none", stPool: "tenant default",
		stCPUs: "auto", stMemory: "auto", stDisk: "auto"}
	for _, s := range []stop{stImage, stPool, stCPUs, stMemory, stDisk, stRepo, stBranch, stSaveAs} {
		t := textinput.New()
		t.Prompt = ""
		t.Placeholder = place[s]
		t.KeyMap.AcceptSuggestion = key.NewBinding(key.WithDisabled())
		t.KeyMap.NextSuggestion = key.NewBinding(key.WithKeys("ctrl+n"))
		t.KeyMap.PrevSuggestion = key.NewBinding(key.WithKeys("ctrl+p"))
		t.Cursor.SetMode(cursor.CursorStatic)
		if sg, ok := sugg[s]; ok {
			t.ShowSuggestions = true
			t.SetSuggestions(sg)
		}
		d.in[s] = &t
	}
	d.fill(ValuesFrom(d.profile.value, cfg.Profiles[d.profile.value]), true)
	d.refreshPoolOptions()
	d.setFocus(stProfile)
	d.resize(rows, cols)
	return d
}

// fill sets the fields from v; with all false, only those not edited.
func (d *Dialog) fill(v DialogValues, all bool) {
	set := func(s stop, val string) {
		if all || !d.edited[s] {
			setText(d.in[s], val)
		}
	}
	set(stImage, v.Image)
	set(stCPUs, v.CPUs)
	set(stMemory, v.Memory)
	set(stDisk, v.Disk)
	set(stRepo, v.Repository)
	set(stBranch, v.Branch)
	if all || !d.edited[stPool] {
		d.pool.value = v.Pool
		setText(d.in[stPool], v.Pool)
	}
	if all || !d.edited[stSize] {
		d.size.value = v.Size
	}
	d.refreshPoolOptions()
}

func setText(t *textinput.Model, s string) {
	t.SetValue(s)
	t.CursorEnd()
	// Matches the suggestions to the new value.
	t.SetSuggestions(t.AvailableSuggestions())
}

func (d *Dialog) resize(rows, cols int) {
	d.rows, d.cols = rows, cols
	d.bg = d.host.Background(rows, cols)
	vw := d.valueWidth()
	for s, t := range d.in {
		switch s {
		case stCPUs, stMemory, stDisk:
			t.Width = max(3, (vw-24)/3)
		default:
			t.Width = max(1, vw-1)
		}
	}
}

func (d *Dialog) valueWidth() int { return max(1, innerWidth(d.cols)-labelWidth) }

// Init loads the pools.
func (d *Dialog) Init() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(d.ctx, poolsFor)
		defer cancel()
		p, err := d.host.Pools(ctx)
		return poolsMsg{p, err}
	}
}

func (d *Dialog) finished() bool { return d.Done }

// poolValue is the pool chosen: from the list, or typed when it could not
// be loaded.
func (d *Dialog) poolValue() string {
	if d.poolsState == poolsFailed {
		return d.in[stPool].Value()
	}
	return d.pool.value
}

// shaped reports whether the chosen pool has sizes (false until the
// pools are known).
func (d *Dialog) shaped() bool {
	if d.poolsState != poolsLoaded {
		return false
	}
	s, _ := Shaped(d.pools, strings.TrimSpace(d.poolValue()))
	return s
}

func (d *Dialog) refreshPoolOptions() {
	opts := []option{{"(tenant default)", ""}}
	for _, p := range d.pools {
		opts = append(opts, option{p.Name, p.Name})
	}
	d.pool.setOptions(opts)
	sopts := []option{{"(pool default)", ""}}
	if _, sizes := Shaped(d.pools, strings.TrimSpace(d.poolValue())); len(sizes) > 0 {
		for _, s := range sizes {
			sopts = append(sopts, option{fmt.Sprintf("%s  %g cpus, %s, %s", s.Name, s.CPUs, gib(s.Memory), gib(s.Disk)), s.Name})
		}
	}
	d.size.setOptions(sopts)
	// The size row changes shape with the pool: the focus stays on it.
	if !slices.Contains(d.stops(), d.focus) {
		switch d.focus {
		case stSize:
			d.setFocus(stCPUs)
		case stCPUs, stMemory, stDisk:
			d.setFocus(stSize)
		}
	}
}

// stops are the places Tab visits, in order.
func (d *Dialog) stops() []stop {
	s := []stop{stProfile, stImage, stPool}
	if d.shaped() || d.poolsState == poolsLoading {
		s = append(s, stSize)
	} else {
		s = append(s, stCPUs, stMemory, stDisk)
	}
	return append(s, stRepo, stBranch, stSpare, stSaveAs, stCreate, stCancel)
}

func (d *Dialog) setFocus(s stop) {
	for _, t := range d.in {
		t.Blur()
	}
	d.focus = s
	if t := d.textAt(s); t != nil {
		t.Focus()
	}
}

// textAt is the text input at s, if s is one.
func (d *Dialog) textAt(s stop) *textinput.Model {
	if s == stPool && d.poolsState != poolsFailed {
		return nil
	}
	return d.in[s]
}

func (d *Dialog) dropdownAt(s stop) *dropdown {
	switch {
	case s == stProfile:
		return d.profile
	case s == stPool && d.poolsState == poolsLoaded:
		return d.pool
	case s == stSize && d.poolsState == poolsLoaded:
		return d.size
	}
	return nil
}

func (d *Dialog) move(delta int) {
	st := d.stops()
	i := slices.Index(st, d.focus)
	d.setFocus(st[(i+delta+len(st))%len(st)])
}

// changed is called after the user changed the field at s.
func (d *Dialog) changed(s stop) {
	d.edited[s] = true
	delete(d.errs, s)
	if s == stCPUs || s == stMemory || s == stDisk {
		delete(d.errs, stSize)
	}
	d.gen++
	d.validating = false
	if d.confirm != nil {
		d.confirm = nil
		d.status, d.statusErr = "", false
	}
}

// Update handles a message.
func (d *Dialog) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		d.resize(m.Height, m.Width)
	case poolsMsg:
		if m.err != nil {
			d.poolsState = poolsFailed
			setText(d.in[stPool], d.pool.value)
			d.setStatus("pools unavailable: "+luxapi.Describe(m.err)+"; type a pool name", true)
		} else {
			d.poolsState, d.pools = poolsLoaded, m.pools
		}
		d.refreshPoolOptions()
		d.setFocus(d.focus)
	case validatedMsg:
		return d, d.validated(m)
	case spinner.TickMsg:
		if d.validating {
			var cmd tea.Cmd
			d.spin, cmd = d.spin.Update(m)
			return d, cmd
		}
	case tea.KeyMsg:
		return d, d.key(m)
	}
	return d, nil
}

func (d *Dialog) key(m tea.KeyMsg) tea.Cmd {
	k := m.String()
	if dd := d.dropdownAt(d.focus); dd != nil && dd.open {
		if k == "ctrl+s" {
			dd.open = false
			return d.submit()
		}
		if dd.key(m) {
			d.picked(d.focus)
		}
		return nil
	}
	switch k {
	case "ctrl+c", "esc":
		return d.cancel()
	case "ctrl+s":
		return d.submit()
	case "tab":
		if d.acceptSuggestion() {
			return nil
		}
		d.move(1)
		return nil
	case "down":
		d.move(1)
		return nil
	case "shift+tab", "up":
		d.move(-1)
		return nil
	case "right":
		if t := d.textAt(d.focus); t != nil && t.Position() == len([]rune(t.Value())) && d.acceptSuggestion() {
			return nil
		}
	case "enter":
		switch d.focus {
		case stCreate:
			return d.submit()
		case stCancel:
			return d.cancel()
		case stSpare:
			d.move(1)
			return nil
		}
		if dd := d.dropdownAt(d.focus); dd != nil {
			dd.openList()
			return nil
		}
		d.move(1)
		return nil
	}
	switch d.focus {
	case stSpare:
		if m.Type == tea.KeySpace {
			d.spare = !d.spare
			d.changed(stSpare)
		}
		return nil
	case stCreate, stCancel:
		switch {
		case k == "left" || k == "right":
			if d.focus == stCreate {
				d.setFocus(stCancel)
			} else {
				d.setFocus(stCreate)
			}
		case m.Type == tea.KeySpace && d.focus == stCreate:
			return d.submit()
		case m.Type == tea.KeySpace:
			return d.cancel()
		}
		return nil
	}
	if dd := d.dropdownAt(d.focus); dd != nil {
		// Typing on a closed list opens it, filtered by what was typed.
		if m.Type == tea.KeyRunes {
			dd.openList()
			dd.key(m)
		}
		return nil
	}
	if t := d.textAt(d.focus); t != nil {
		before := t.Value()
		*t, _ = t.Update(m)
		if t.Value() != before {
			d.changed(d.focus)
			if d.focus == stPool {
				d.refreshPoolOptions()
			}
		}
	}
	return nil
}

// acceptSuggestion completes the focused text field with its suggestion,
// if one extends what was typed.
func (d *Dialog) acceptSuggestion() bool {
	t := d.textAt(d.focus)
	if t == nil || !t.ShowSuggestions {
		return false
	}
	s, v := t.CurrentSuggestion(), t.Value()
	if v == "" || len([]rune(s)) <= len([]rune(v)) || !strings.HasPrefix(strings.ToLower(s), strings.ToLower(v)) {
		return false
	}
	setText(t, s)
	d.changed(d.focus)
	return true
}

// picked is called after an option was picked in the list at s.
func (d *Dialog) picked(s stop) {
	d.changed(s)
	switch s {
	case stProfile:
		name := d.profile.value
		d.fill(ValuesFrom(name, d.cfg.Profiles[name]), false)
		// The profile is not a field the user typed: a later change
		// refills again.
		delete(d.edited, stProfile)
	case stPool:
		d.refreshPoolOptions()
	}
}

func (d *Dialog) cancel() tea.Cmd {
	d.Done, d.Request = true, nil
	return tea.Quit
}

func (d *Dialog) setStatus(s string, isErr bool) { d.status, d.statusErr = s, isErr }

// submit checks the fields, then asks lux's dry run; a second press after
// a "would not start now" answer creates the window.
func (d *Dialog) submit() tea.Cmd {
	if d.validating {
		return nil
	}
	if d.confirm != nil {
		d.Request, d.Done = d.confirm, true
		return tea.Quit
	}
	if d.poolsState == poolsLoading {
		d.setStatus("pools are still loading…", false)
		return nil
	}
	clear(d.errs)
	v := d.values()
	prof, err := v.Profile(d.cfg.Profiles[v.Base], d.shaped())
	if err != nil {
		var general []string
		first := stCancel
		st := d.stops()
		for f, msgs := range FieldErrors(err) {
			s, ok := fieldStop[f]
			if !ok {
				general = append(general, msgs...)
				continue
			}
			d.errs[s] = strings.Join(msgs, "; ")
			if slices.Index(st, s) >= 0 && slices.Index(st, s) < slices.Index(st, first) {
				first = s
			}
		}
		msg := "fix the marked fields"
		if len(general) > 0 {
			slices.Sort(general)
			msg = strings.Join(general, "; ")
		}
		d.setStatus(msg, true)
		if first != stCancel {
			d.setFocus(first)
		}
		return nil
	}
	d.validating = true
	d.setStatus("checking with lux…", false)
	gen := d.gen
	req := &mux.NewWindowRequest{ProfileName: v.Base, Profile: prof, SaveAs: v.SaveAs}
	return tea.Batch(d.spin.Tick, func() tea.Msg {
		ctx, cancel := context.WithTimeout(d.ctx, validateFor)
		defer cancel()
		ans, err := d.host.Validate(ctx, v.Base, prof)
		return validatedMsg{gen: gen, req: req, ans: ans, err: err}
	})
}

func (d *Dialog) validated(m validatedMsg) tea.Cmd {
	if m.gen != d.gen || !d.validating {
		return nil
	}
	d.validating = false
	switch {
	case m.err != nil:
		d.refused(luxapi.Describe(m.err))
	case m.ans == nil || !m.ans.Valid:
		d.refused("lux's dry run did not find it valid")
	case !m.ans.WouldStartNow:
		d.confirm = m.req
		d.setStatus("valid, but would not start now (it waits for capacity): "+strings.Join(Blockers(m.ans), "; ")+
			". Create again to create it anyway", false)
		d.setFocus(stCreate)
	default:
		d.Request, d.Done = m.req, true
		return tea.Quit
	}
	return nil
}

// refused shows lux's refusal and marks the fields it names.
func (d *Dialog) refused(msg string) {
	d.setStatus("lux refuses it: "+msg, true)
	low := strings.ToLower(msg)
	mark := func(s stop, words ...string) {
		for _, w := range words {
			if strings.Contains(low, w) {
				d.errs[s] = "named in lux's refusal (below)"
				return
			}
		}
	}
	mark(stImage, "image")
	mark(stPool, "pool")
	mark(stSize, "size", "resources", "cpus", "memory", "disk")
	mark(stRepo, "git", "repositor")
}

func (d *Dialog) values() DialogValues {
	return DialogValues{
		Base: d.profile.value, Image: d.in[stImage].Value(), Pool: d.poolValue(), Size: d.size.value,
		CPUs: d.in[stCPUs].Value(), Memory: d.in[stMemory].Value(), Disk: d.in[stDisk].Value(),
		Repository: d.in[stRepo].Value(), Branch: d.in[stBranch].Value(), WarmSpare: d.spare, SaveAs: d.in[stSaveAs].Value(),
	}
}

// secretNames are the secrets the window's Run will carry.
func (d *Dialog) secretNames() []string {
	return d.values().SecretNames(d.cfg.Profiles[d.profile.value])
}

var stopLabel = map[stop]string{
	stProfile: "Profile", stImage: "Image", stPool: "Pool", stSize: "Size", stRepo: "Repository",
	stBranch: "Branch", stSpare: "Warm spare", stSaveAs: "Save as profile",
}

// View draws the dialog.
func (d *Dialog) View() string {
	if d.Done {
		return ""
	}
	if tooSmall(d.rows, d.cols) {
		return tooSmallView(d.rows, d.cols)
	}
	inner := innerWidth(d.cols)
	lines, at := d.fieldLines()
	footer := []string{""}
	status := d.status
	if d.validating {
		status = d.spin.View() + " " + status
	}
	if status != "" {
		wrapped := strings.Split(ansi.Wrap(status, inner, " "), "\n")
		if len(wrapped) > 3 {
			wrapped = append(wrapped[:2], ansi.Truncate(strings.Join(wrapped[2:], " "), inner, "…"))
		}
		for _, l := range wrapped {
			if d.statusErr {
				l = styleErr.Render(l)
			}
			footer = append(footer, l)
		}
	}
	footer = append(footer, styleDim.Render(d.helpLine()))
	avail := max(1, maxBody(d.rows)-len(footer))
	if len(lines) <= avail {
		d.scroll = 0
	} else {
		r := at[d.focus]
		if r[0] < d.scroll {
			d.scroll = r[0]
		}
		if r[1] > d.scroll+avail {
			d.scroll = r[1] - avail
		}
		d.scroll = min(max(d.scroll, 0), len(lines)-avail)
		lines = lines[d.scroll : d.scroll+avail]
	}
	return frame(d.rows, d.cols, d.bg, "New window", append(lines, footer...))
}

func (d *Dialog) helpLine() string {
	if dd := d.dropdownAt(d.focus); dd != nil && dd.open {
		return "type to filter · ↑/↓ choose · enter pick · esc close list"
	}
	if innerWidth(d.cols) < 72 {
		return "tab/↑↓ move · enter open · ctrl-s create · esc cancel"
	}
	return "tab/↓ ⇧tab/↑ move · enter open · space toggle · ctrl-s create · esc cancel"
}

// fieldLines are the form's lines, and for each stop the range of lines
// it covers (to scroll it into view).
func (d *Dialog) fieldLines() ([]string, map[stop][2]int) {
	var out []string
	at := map[stop][2]int{}
	vw := d.valueWidth()
	pad := strings.Repeat(" ", labelWidth)
	row := func(s stop, focused bool, value string, extra ...string) {
		start := len(out)
		lab := stopLabel[s]
		mark := "  "
		switch {
		case focused:
			mark = styleFocus.Render("▸ ")
			lab = styleFocus.Render(fitLine(lab, labelWidth-2))
		case d.errs[s] != "":
			lab = styleErr.Render(fitLine(lab, labelWidth-2))
		default:
			lab = fitLine(lab, labelWidth-2)
		}
		out = append(out, mark+lab+value)
		for _, e := range extra {
			out = append(out, pad+e)
		}
		at[s] = [2]int{start, len(out)}
	}
	errLine := func(s stop) []string {
		if e := d.errs[s]; e != "" {
			return []string{styleErr.Render("✗ " + e)}
		}
		return nil
	}
	ddRow := func(s stop, dd *dropdown) {
		focused := d.focus == s
		extra := []string{}
		if dd.open {
			extra = dd.listLines(vw)
		}
		row(s, focused, dd.view(vw, focused), append(extra, errLine(s)...)...)
	}
	textRow := func(s stop) {
		row(s, d.focus == s, d.in[s].View(), errLine(s)...)
	}

	ddRow(stProfile, d.profile)
	textRow(stImage)
	switch d.poolsState {
	case poolsLoading:
		row(stPool, d.focus == stPool, styleDim.Render("loading…"), errLine(stPool)...)
	case poolsFailed:
		textRow(stPool)
	default:
		ddRow(stPool, d.pool)
	}
	switch {
	case d.poolsState == poolsLoading:
		row(stSize, d.focus == stSize, styleDim.Render("loading…"), errLine(stSize)...)
	case d.shaped():
		ddRow(stSize, d.size)
	default:
		part := func(s stop, name string) string {
			if d.focus == s {
				name = styleFocus.Render(name)
			} else if d.errs[s] != "" {
				name = styleErr.Render(name)
			}
			return name + " " + d.in[s].View()
		}
		focused := d.focus == stCPUs || d.focus == stMemory || d.focus == stDisk
		var errs []string
		for _, s := range []stop{stSize, stCPUs, stMemory, stDisk} {
			errs = append(errs, errLine(s)...)
		}
		row(stSize, focused, part(stCPUs, "cpus")+"  "+part(stMemory, "memory")+"  "+part(stDisk, "disk"), errs...)
		for _, s := range []stop{stCPUs, stMemory, stDisk} {
			at[s] = at[stSize]
		}
	}
	textRow(stRepo)
	textRow(stBranch)
	secrets := "none"
	if n := d.secretNames(); len(n) > 0 {
		secrets = strings.Join(n, ", ")
	}
	out = append(out, "  "+fitLine("Secrets", labelWidth-2)+styleDim.Render(ansi.Truncate(secrets, vw, "…")))
	box := "[ ]"
	if d.spare {
		box = "[x]"
	}
	if d.focus == stSpare {
		box = styleReverse.Render(box)
	}
	row(stSpare, d.focus == stSpare, box+styleDim.Render("  billed while it waits"))
	textRow(stSaveAs)
	out = append(out, "")
	btn := func(s stop, name string) string {
		b := "[ " + name + " ]"
		if d.focus == s {
			return styleReverse.Render(b)
		}
		return b
	}
	start := len(out)
	out = append(out, pad+btn(stCreate, "Create")+"  "+btn(stCancel, "Cancel"))
	at[stCreate] = [2]int{start, len(out)}
	at[stCancel] = at[stCreate]
	return out, at
}

// option is one entry of a dropdown: what it shows and its value.
type option struct{ label, value string }

// dropdown is a list of options, one of them chosen, opened to pick.
type dropdown struct {
	opts   []option
	value  string
	open   bool
	filter string
	cursor int // into visible()
}

// setOptions replaces the options; a chosen value they lack is kept as an
// option of its own, so it is never dropped unseen.
func (dd *dropdown) setOptions(opts []option) {
	dd.opts = opts
	for _, o := range opts {
		if o.value == dd.value {
			return
		}
	}
	dd.opts = append(dd.opts, option{dd.value + "  (not listed)", dd.value})
}

func (dd *dropdown) visible() []option {
	if dd.filter == "" {
		return dd.opts
	}
	f := strings.ToLower(dd.filter)
	var out []option
	for _, o := range dd.opts {
		if strings.Contains(strings.ToLower(o.label), f) {
			out = append(out, o)
		}
	}
	return out
}

func (dd *dropdown) label() string {
	for _, o := range dd.opts {
		if o.value == dd.value {
			return o.label
		}
	}
	return dd.value
}

func (dd *dropdown) openList() {
	dd.open, dd.filter, dd.cursor = true, "", 0
	for i, o := range dd.opts {
		if o.value == dd.value {
			dd.cursor = i
		}
	}
}

// key handles a key while the list is open; picked reports a choice.
func (dd *dropdown) key(m tea.KeyMsg) (picked bool) {
	switch m.String() {
	case "esc":
		dd.open = false
	case "up", "shift+tab":
		dd.cursor--
	case "down", "tab":
		dd.cursor++
	case "enter":
		if v := dd.visible(); dd.cursor >= 0 && dd.cursor < len(v) {
			dd.value = v[dd.cursor].value
			dd.open = false
			return true
		}
		dd.open = false
	case "backspace":
		if r := []rune(dd.filter); len(r) > 0 {
			dd.filter = string(r[:len(r)-1])
			dd.cursor = 0
		}
	default:
		switch m.Type {
		case tea.KeyRunes:
			dd.filter += string(m.Runes)
			dd.cursor = 0
		case tea.KeySpace:
			dd.filter += " "
			dd.cursor = 0
		}
	}
	dd.cursor = min(max(dd.cursor, 0), max(len(dd.visible())-1, 0))
	return false
}

func (dd *dropdown) view(w int, focused bool) string {
	s := "[ " + fitLine(dd.label(), max(1, min(w, 48)-6)) + " ▾ ]"
	if focused {
		return styleFocus.Render(s)
	}
	return s
}

// listLines are the open list's lines: the filter, then up to
// dropdownRows options around the cursor.
func (dd *dropdown) listLines(w int) []string {
	out := []string{styleDim.Render("filter: ") + dd.filter + "_"}
	v := dd.visible()
	if len(v) == 0 {
		return append(out, styleDim.Render("  (no match)"))
	}
	start := min(max(0, dd.cursor-dropdownRows/2), max(0, len(v)-dropdownRows))
	for i := start; i < min(len(v), start+dropdownRows); i++ {
		l := "  " + v[i].label
		if i == dd.cursor {
			l = styleReverse.Render(fitLine("› "+v[i].label, max(1, w-1)))
		}
		out = append(out, l)
	}
	return out
}
