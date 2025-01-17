package main

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/zyedidia/tcell"
)

// The ViewType defines what kind of view this is
type ViewType struct {
	Kind     int
	Readonly bool // The file cannot be edited
	Scratch  bool // The file cannot be saved
}

var (
	vtDefault = ViewType{0, false, false}
	vtHelp    = ViewType{1, true, true}
	vtLog     = ViewType{2, true, true}
	vtScratch = ViewType{3, false, true}
	vtRaw     = ViewType{4, true, true}
	vtTerm    = ViewType{5, true, true}
)

// The View struct stores information about a view into a buffer.
// It stores information about the cursor, and the viewport
// that the user sees the buffer from.
type View struct {
	// A pointer to the buffer's cursor for ease of access
	Cursor *Cursor

	// The topmost line, used for vertical scrolling
	Topline int
	// The leftmost column, used for horizontal scrolling
	leftCol int

	// Specifies whether or not this view holds a help buffer
	Type ViewType

	// Actual width and height
	Width  int
	Height int

	LockWidth  bool
	LockHeight bool

	// Where this view is located
	x, y int

	// How much to offset because of line numbers
	lineNumOffset int

	// Holds the list of gutter messages
	messages map[string][]GutterMessage

	// This is the index of this view in the views array
	Num int
	// What tab is this view stored in
	TabNum int

	// The buffer
	Buf *Buffer
	// The statusline
	sline *Statusline

	// Since tcell doesn't differentiate between a mouse release event
	// and a mouse move event with no keys pressed, we need to keep
	// track of whether or not the mouse was pressed (or not released) last event to determine
	// mouse release events
	mouseReleased bool

	// We need to keep track of insert key press toggle
	isOverwriteMode bool
	// This stores when the last click was
	// This is useful for detecting double and triple clicks
	lastClickTime time.Time
	lastLoc       Loc

	// lastCutTime stores when the last ctrl+k was issued.
	// It is used for clearing the clipboard to replace it with fresh cut lines.
	lastCutTime time.Time

	// freshClip returns true if the clipboard has never been pasted.
	freshClip bool

	// Was the last mouse event actually a double click?
	// Useful for detecting triple clicks -- if a double click is detected
	// but the last mouse event was actually a double click, it's a triple click
	doubleClick bool
	// Same here, just to keep track for mouse move events
	tripleClick bool

	// The cellview used for displaying and syntax highlighting
	cellview *CellView

	splitNode *LeafNode

	// The scrollbar
	scrollbar *ScrollBar

	// Autocomplete function
	Completer *Completer

	// Virtual terminal
	term *Terminal
}

// NewView returns a new fullscreen view
func NewView(buf *Buffer) *View {
	screenW, screenH := screen.Size()
	return NewViewWidthHeight(buf, screenW, screenH)
}

// NewViewWidthHeight returns a new view with the specified width and height
// Note that w and h are raw column and row values
func NewViewWidthHeight(buf *Buffer, w, h int) *View {
	v := new(View)

	v.x, v.y = 0, 0

	v.Width = w
	v.Height = h
	v.cellview = new(CellView)

	v.ToggleTabbar()

	v.OpenBuffer(buf)

	v.messages = make(map[string][]GutterMessage)

	v.sline = &Statusline{
		view: v,
	}

	v.scrollbar = &ScrollBar{
		view: v,
	}

	if v.Buf.Settings["statusline"].(bool) {
		v.Height--
	}

	v.term = new(Terminal)

	for pl := range loadedPlugins {
		_, err := Call(pl+".onViewOpen", v)
		if err != nil && !strings.HasPrefix(err.Error(), "function does not exist") {
			TermMessage(err)
			continue
		}
	}

	// Load the autocompleter, based on the filetype.
	v.Completer = NewCompleterForView(v)

	return v
}

// ToggleStatusLine creates an extra row for the statusline if necessary
func (v *View) ToggleStatusLine() {
	if v.Buf.Settings["statusline"].(bool) {
		v.Height--
	} else {
		v.Height++
	}
}

// StartTerminal execs a command in this view
func (v *View) StartTerminal(execCmd []string, wait bool, getOutput bool, luaCallback string) error {
	err := v.term.Start(execCmd, v, getOutput)
	v.term.wait = wait
	v.term.callback = luaCallback
	if err == nil {
		v.term.Resize(v.Width, v.Height)
		v.Type = vtTerm
	}
	return err
}

// CloseTerminal shuts down the tty running in this view
// and returns it to the default view type
func (v *View) CloseTerminal() {
	v.term.Stop()
}

// ToggleTabbar creates an extra row for the tabbar if necessary
func (v *View) ToggleTabbar() {
	if len(tabs) > 1 {
		if v.y == 0 {
			// Include one line for the tab bar at the top
			v.Height--
			v.y = 1
		}
	} else {
		if v.y == 1 {
			v.y = 0
			v.Height++
		}
	}
}

func (v *View) paste(clip string) {
	if v.Buf.Settings["smartpaste"].(bool) {
		if v.Cursor.X > 0 && GetLeadingWhitespace(strings.TrimLeft(clip, "\r\n")) == "" {
			leadingWS := GetLeadingWhitespace(v.Buf.Line(v.Cursor.Y))
			clip = strings.Replace(clip, "\n", "\n"+leadingWS, -1)
		}
	}

	if v.Cursor.HasSelection() {
		v.Cursor.DeleteSelection()
		v.Cursor.ResetSelection()
	}
	
	v.Buf.Insert(v.Cursor.Loc, clip)
	// v.Cursor.Loc = v.Cursor.Loc.Move(Count(clip), v.Buf)
	v.freshClip = false
	messenger.Message("Pasted clipboard")
}

// ScrollUp scrolls the view up n lines (if possible)
func (v *View) ScrollUp(n int) {
	// Try to scroll by n but if it would overflow, scroll by 1
	if v.Topline-n >= 0 {
		v.Topline -= n
	} else if v.Topline > 0 {
		v.Topline--
	}
}

// ScrollDown scrolls the view down n lines (if possible)
func (v *View) ScrollDown(n int) {
	// Try to scroll by n but if it would overflow, scroll by 1
	if v.Topline+n <= v.Buf.NumLines {
		v.Topline += n
	} else if v.Topline < v.Buf.NumLines-1 {
		v.Topline++
	}
}

// CanClose returns whether or not the view can be closed
// If there are unsaved changes, the user will be asked if the view can be closed
// causing them to lose the unsaved changes
func (v *View) CanClose() bool {
	if v.Type == vtDefault && v.Buf.Modified() {
		var choice bool
		var canceled bool
		if v.Buf.Settings["autosave"].(bool) {
			choice = true
		} else {
			choice, canceled = messenger.YesNoPrompt("Save changes to " + v.Buf.GetName() + " before closing? (y,n,esc) ")
		}
		if !canceled {
			//if char == 'y' {
			if choice {
				v.Save(true)
			}
		} else {
			return false
		}
	}
	return true
}

// OpenBuffer opens a new buffer in this view.
// This resets the topline, event handler and cursor.
func (v *View) OpenBuffer(buf *Buffer) {
	screen.Clear()
	v.CloseBuffer()
	v.Buf = buf
	v.Cursor = &buf.Cursor
	v.Topline = 0
	v.leftCol = 0
	v.Cursor.ResetSelection()
	v.Relocate()
	v.Center(false)
	v.messages = make(map[string][]GutterMessage)

	// Set mouseReleased to true because we assume the mouse is not being pressed when
	// the editor is opened
	v.mouseReleased = true
	// Set isOverwriteMode to false, because we assume we are in the default mode when editor
	// is opened
	v.isOverwriteMode = false
	v.lastClickTime = time.Time{}

	GlobalPluginCall("onBufferOpen", v.Buf)
	GlobalPluginCall("onViewOpen", v)
}

// Open opens the given file in the view
func (v *View) Open(path string) {
	buf, err := NewBufferFromFile(path)
	if err != nil {
		messenger.Error(err)
		return
	}
	v.OpenBuffer(buf)
}

// CloseBuffer performs any closing functions on the buffer
func (v *View) CloseBuffer() {
	if v.Buf != nil {
		v.Buf.Serialize()
	}
}

// ReOpen reloads the current buffer
func (v *View) ReOpen() {
	if v.CanClose() {
		screen.Clear()
		v.Buf.ReOpen()
		v.Relocate()
	}
}

// HSplit opens a horizontal split with the given buffer
func (v *View) HSplit(buf *Buffer) {
	i := 0
	if v.Buf.Settings["splitbottom"].(bool) {
		i = 1
	}
	v.splitNode.HSplit(buf, v.Num+i)
}

// VSplit opens a vertical split with the given buffer
func (v *View) VSplit(buf *Buffer) {
	i := 0
	if v.Buf.Settings["splitright"].(bool) {
		i = 1
	}
	v.splitNode.VSplit(buf, v.Num+i)
}

// HSplitIndex opens a horizontal split with the given buffer at the given index
func (v *View) HSplitIndex(buf *Buffer, splitIndex int) {
	v.splitNode.HSplit(buf, splitIndex)
}

// VSplitIndex opens a vertical split with the given buffer at the given index
func (v *View) VSplitIndex(buf *Buffer, splitIndex int) {
	v.splitNode.VSplit(buf, splitIndex)
}

// GetSoftWrapLocation gets the location of a visual click on the screen and converts it to col,line
func (v *View) GetSoftWrapLocation(vx, vy int) (int, int) {
	if !v.Buf.Settings["softwrap"].(bool) {
		if vy >= v.Buf.NumLines {
			vy = v.Buf.NumLines - 1
		}
		vx = v.Cursor.GetCharPosInLine(vy, vx)
		return vx, vy
	}

	screenX, screenY := 0, v.Topline
	for lineN := v.Topline; lineN < v.Bottomline(); lineN++ {
		line := v.Buf.Line(lineN)
		if lineN >= v.Buf.NumLines {
			return 0, v.Buf.NumLines - 1
		}

		colN := 0
		for _, ch := range line {
			if screenX >= v.Width-v.lineNumOffset {
				screenX = 0
				screenY++
			}

			if screenX == vx && screenY == vy {
				return colN, lineN
			}

			if ch == '\t' {
				screenX += int(v.Buf.Settings["tabsize"].(float64)) - 1
			}

			screenX++
			colN++
		}
		if screenY == vy {
			return colN, lineN
		}
		screenX = 0
		screenY++
	}

	return 0, 0
}

// Bottomline returns the line number of the lowest line in the view
// You might think that this is obviously just v.Topline + v.Height
// but if softwrap is enabled things get complicated since one buffer
// line can take up multiple lines in the view
func (v *View) Bottomline() int {
	if !v.Buf.Settings["softwrap"].(bool) {
		return v.Topline + v.Height
	}

	screenX, screenY := 0, 0
	numLines := 0
	for lineN := v.Topline; lineN < v.Topline+v.Height; lineN++ {
		line := v.Buf.Line(lineN)

		colN := 0
		for _, ch := range line {
			if screenX >= v.Width-v.lineNumOffset {
				screenX = 0
				screenY++
			}

			if ch == '\t' {
				screenX += int(v.Buf.Settings["tabsize"].(float64)) - 1
			}

			screenX++
			colN++
		}
		screenX = 0
		screenY++
		numLines++

		if screenY >= v.Height {
			break
		}
	}
	return numLines + v.Topline
}

// Relocate moves the view window so that the cursor is in view
// This is useful if the user has scrolled far away, and then starts typing
func (v *View) Relocate() bool {
	height := v.Bottomline() - v.Topline
	ret := false
	cy := v.Cursor.Y
	scrollmargin := int(v.Buf.Settings["scrollmargin"].(float64))
	if cy < v.Topline+scrollmargin && cy > scrollmargin-1 {
		v.Topline = cy - scrollmargin
		ret = true
	} else if cy < v.Topline {
		v.Topline = cy
		ret = true
	}
	if cy > v.Topline+height-1-scrollmargin && cy < v.Buf.NumLines-scrollmargin {
		v.Topline = cy - height + 1 + scrollmargin
		ret = true
	} else if cy >= v.Buf.NumLines-scrollmargin && cy >= height {
		v.Topline = v.Buf.NumLines - height
		ret = true
	}

	if !v.Buf.Settings["softwrap"].(bool) {
		cx := v.Cursor.GetVisualX()
		if cx < v.leftCol {
			v.leftCol = cx
			ret = true
		}
		if cx+v.lineNumOffset+1 > v.leftCol+v.Width {
			v.leftCol = cx - v.Width + v.lineNumOffset + 1
			ret = true
		}
	}
	return ret
}

// GetMouseClickLocation gets the location in the buffer from a mouse click
// on the screen
func (v *View) GetMouseClickLocation(x, y int) (int, int) {
	x -= v.lineNumOffset - v.leftCol + v.x
	y += v.Topline - v.y

	if y-v.Topline > v.Height-1 {
		v.ScrollDown(1)
		y = v.Height + v.Topline - 1
	}
	if y < 0 {
		y = 0
	}
	if x < 0 {
		x = 0
	}

	newX, newY := v.GetSoftWrapLocation(x, y)
	if newX > Count(v.Buf.Line(newY)) {
		newX = Count(v.Buf.Line(newY))
	}

	return newX, newY
}

// MoveToMouseClick moves the cursor to location x, y assuming x, y were given
// by a mouse click
func (v *View) MoveToMouseClick(x, y int) {
	if y-v.Topline > v.Height-1 {
		v.ScrollDown(1)
		y = v.Height + v.Topline - 1
	}
	if y < 0 {
		y = 0
	}
	if x < 0 {
		x = 0
	}

	x, y = v.GetSoftWrapLocation(x, y)
	if x > Count(v.Buf.Line(y)) {
		x = Count(v.Buf.Line(y))
	}
	v.Cursor.X = x
	v.Cursor.Y = y
	v.Cursor.LastVisualX = v.Cursor.GetVisualX()
}

// ExecuteActions executes the supplied actions
func (v *View) ExecuteActions(actions []func(*View, bool) bool) bool {
	relocate := false
	readonlyBindingsList := []string{"Delete", "Insert", "Backspace", "Cut", "Play", "Paste", "Move", "Add", "DuplicateLine", "Macro"}
	for _, action := range actions {
		readonlyBindingsResult := false
		funcName := ShortFuncName(action)
		curv := CurView()
		if curv.Type.Readonly == true {
			// check for readonly and if true only let key bindings get called if they do not change the contents.
			for _, readonlyBindings := range readonlyBindingsList {
				if strings.Contains(funcName, readonlyBindings) {
					readonlyBindingsResult = true
				}
			}
		}
		if !readonlyBindingsResult {
			// call the key binding
			relocate = action(curv, true) || relocate
			// Macro
			if funcName != "ToggleMacro" && funcName != "PlayMacro" {
				if recordingMacro {
					curMacro = append(curMacro, action)
				}
			}
		}
	}

	return relocate
}

// SetCursor sets the view's and buffer's cursor
func (v *View) SetCursor(c *Cursor) bool {
	if c == nil {
		return false
	}
	v.Cursor = c
	v.Buf.curCursor = c.Num

	return true
}

// HandleEvent handles an event passed by the main loop
func (v *View) HandleEvent(event tcell.Event) {
	if v.Type == vtTerm {
		v.term.HandleEvent(event)
		return
	}

	if v.Type == vtRaw {
		v.Buf.Insert(v.Cursor.Loc, reflect.TypeOf(event).String()[7:])
		v.Buf.Insert(v.Cursor.Loc, fmt.Sprintf(": %q\n", event.EscSeq()))

		switch e := event.(type) {
		case *tcell.EventKey:
			if e.Key() == tcell.KeyCtrlQ {
				v.Quit(true)
			}
		}

		return
	}

	// This bool determines whether the view is relocated at the end of the function
	// By default it's true because most events should cause a relocate
	relocate := true

	v.Buf.CheckModTime()

	switch e := event.(type) {
	case *tcell.EventRaw:
		for key, actions := range bindings {
			if key.keyCode == -1 {
				if e.EscSeq() == key.escape {
					for _, c := range v.Buf.cursors {
						ok := v.SetCursor(c)
						if !ok {
							break
						}
						relocate = false
						relocate = v.ExecuteActions(actions) || relocate
					}
					v.SetCursor(&v.Buf.Cursor)
					v.Buf.MergeCursors()
					break
				}
			}
		}
	case *tcell.EventKey:
		// See whether the autocomplete should take over the keys.
		if v.Completer.HandleEvent(e.Key()) {
			// The completer has taken over the key, so break.
			break
		}

		// Check first if input is a key binding, if it is we 'eat' the input and don't insert a rune
		isBinding := false
		for key, actions := range bindings {
			if e.Key() == key.keyCode {
				if e.Key() == tcell.KeyRune {
					if e.Rune() != key.r {
						continue
					}
				}
				if e.Modifiers() == key.modifiers {
					for _, c := range v.Buf.cursors {
						ok := v.SetCursor(c)
						if !ok {
							break
						}
						relocate = false
						isBinding = true
						relocate = v.ExecuteActions(actions) || relocate
					}
					v.SetCursor(&v.Buf.Cursor)
					v.Buf.MergeCursors()
					break
				}
			}
		}

		if !isBinding && e.Key() == tcell.KeyRune {
			// Check viewtype if readonly don't insert a rune (readonly help and log view etc.)
			if !v.Type.Readonly {
				for _, c := range v.Buf.cursors {
					v.SetCursor(c)

					// Insert a character
					if v.Cursor.HasSelection() {
						v.Cursor.DeleteSelection()
						v.Cursor.ResetSelection()
					}

					if v.isOverwriteMode {
						next := v.Cursor.Loc
						next.X++
						v.Buf.Replace(v.Cursor.Loc, next, string(e.Rune()))
					} else {
						v.Buf.Insert(v.Cursor.Loc, string(e.Rune()))
					}

					// Allow the completer to access the rune.
					err := v.Completer.Process(e.Rune())
					if err != nil {
						TermMessage(err)
					}

					for pl := range loadedPlugins {
						_, err := Call(pl+".onRune", string(e.Rune()), v)
						if err != nil && !strings.HasPrefix(err.Error(), "function does not exist") {
							TermMessage(err)
						}
					}

					if recordingMacro {
						curMacro = append(curMacro, e.Rune())
					}
				}
				v.SetCursor(&v.Buf.Cursor)
			}
		}
	case *tcell.EventPaste:
		// Check viewtype if readonly don't paste (readonly help and log view etc.)
		if v.Type.Readonly == false {
			if !PreActionCall("Paste", v) {
				break
			}

			for _, c := range v.Buf.cursors {
				v.SetCursor(c)
				v.paste(e.Text())
			}
			v.SetCursor(&v.Buf.Cursor)

			PostActionCall("Paste", v)
		}
	case *tcell.EventMouse:
		// Don't relocate for mouse events
		relocate = false

		button := e.Buttons()

		for key, actions := range bindings {
			if button == key.buttons && e.Modifiers() == key.modifiers {
				for _, c := range v.Buf.cursors {
					ok := v.SetCursor(c)
					if !ok {
						break
					}
					relocate = v.ExecuteActions(actions) || relocate
				}
				v.SetCursor(&v.Buf.Cursor)
				v.Buf.MergeCursors()
			}
		}

		for key, actions := range mouseBindings {
			if button == key.buttons && e.Modifiers() == key.modifiers {
				for _, action := range actions {
					action(v, true, e)
				}
			}
		}

		switch button {
		case tcell.ButtonNone:
			// Mouse event with no click
			if !v.mouseReleased {
				// Mouse was just released

				x, y := e.Position()
				x -= v.lineNumOffset - v.leftCol + v.x
				y += v.Topline - v.y

				// Relocating here isn't really necessary because the cursor will
				// be in the right place from the last mouse event
				// However, if we are running in a terminal that doesn't support mouse motion
				// events, this still allows the user to make selections, except only after they
				// release the mouse

				if !v.doubleClick && !v.tripleClick {
					v.MoveToMouseClick(x, y)
					v.Cursor.SetSelectionEnd(v.Cursor.Loc)
					v.Cursor.CopySelection("primary")
				}
				v.mouseReleased = true
			}
		}
	}

	if relocate {
		v.Relocate()
		// We run relocate again because there's a bug with relocating with softwrap
		// when for example you jump to the bottom of the buffer and it tries to
		// calculate where to put the topline so that the bottom line is at the bottom
		// of the terminal and it runs into problems with visual lines vs real lines.
		// This is (hopefully) a temporary solution
		v.Relocate()
	}

	// Check to see whether the cursor has moved out of the autocomplete range,
	// and the completer should exit.
	v.Completer.DeactivateIfOutOfBounds()
}

func (v *View) mainCursor() bool {
	return v.Buf.curCursor == len(v.Buf.cursors)-1
}

// GutterMessage creates a message in this view's gutter
func (v *View) GutterMessage(section string, lineN int, msg string, kind int) {
	lineN--
	gutterMsg := GutterMessage{
		lineNum: lineN,
		msg:     msg,
		kind:    kind,
	}
	for _, v := range v.messages {
		for _, gmsg := range v {
			if gmsg.lineNum == lineN {
				return
			}
		}
	}
	messages := v.messages[section]
	v.messages[section] = append(messages, gutterMsg)
}

// ClearGutterMessages clears all gutter messages from a given section
func (v *View) ClearGutterMessages(section string) {
	v.messages[section] = []GutterMessage{}
}

// ClearAllGutterMessages clears all the gutter messages
func (v *View) ClearAllGutterMessages() {
	for k := range v.messages {
		v.messages[k] = []GutterMessage{}
	}
}

// Opens the given help page in a new horizontal split
func (v *View) openHelp(helpPage string) {
	if data, err := FindRuntimeFile(RTHelp, helpPage).Data(); err != nil {
		TermMessage("Unable to load help text", helpPage, "\n", err)
	} else {
		helpBuffer := NewBufferFromString(string(data), helpPage+".md")
		helpBuffer.name = "Help"

		if v.Type == vtHelp {
			v.OpenBuffer(helpBuffer)
		} else {
			v.HSplit(helpBuffer)
			CurView().Type = vtHelp
		}
	}
}

// DisplayView draws the view to the screen
func (v *View) DisplayView() {
	if v.Type == vtTerm {
		v.term.Display()
		return
	}

	if v.Buf.Settings["softwrap"].(bool) && v.leftCol != 0 {
		v.leftCol = 0
	}

	if v.Type == vtLog || v.Type == vtRaw {
		// Log or raw views should always follow the cursor...
		v.Relocate()
	}

	// We need to know the string length of the largest line number
	// so we can pad appropriately when displaying line numbers
	maxLineNumLength := len(strconv.Itoa(v.Buf.NumLines))

	if v.Buf.Settings["ruler"] == true {
		// + 1 for the little space after the line number
		v.lineNumOffset = maxLineNumLength + 1
	} else {
		v.lineNumOffset = 0
	}

	// We need to add to the line offset if there are gutter messages
	var hasGutterMessages bool
	for _, v := range v.messages {
		if len(v) > 0 {
			hasGutterMessages = true
		}
	}
	if hasGutterMessages {
		v.lineNumOffset += 2
	}

	divider := 0
	if v.x != 0 {
		// One space for the extra split divider
		v.lineNumOffset++
		divider = 1
	}

	xOffset := v.x + v.lineNumOffset
	yOffset := v.y

	height := v.Height
	width := v.Width
	left := v.leftCol
	top := v.Topline

	v.cellview.Draw(v.Buf, top, height, left, width-v.lineNumOffset)

	screenX := v.x
	realLineN := top - 1
	visualLineN := 0
	var line []*Char
	for visualLineN, line = range v.cellview.lines {
		var firstChar *Char
		if len(line) > 0 {
			firstChar = line[0]
		}

		var softwrapped bool
		if firstChar != nil {
			if firstChar.realLoc.Y == realLineN {
				softwrapped = true
			}
			realLineN = firstChar.realLoc.Y
		} else {
			realLineN++
		}

		colorcolumn := int(v.Buf.Settings["colorcolumn"].(float64))
		if colorcolumn != 0 && xOffset+colorcolumn-v.leftCol < v.Width {
			style := GetColor("color-column")
			fg, _, _ := style.Decompose()
			st := defStyle.Background(fg)
			screen.SetContent(xOffset+colorcolumn-v.leftCol, yOffset+visualLineN, ' ', nil, st)
		}

		screenX = v.x

		// If there are gutter messages we need to display the '>>' symbol here
		if hasGutterMessages {
			// msgOnLine stores whether or not there is a gutter message on this line in particular
			msgOnLine := false
			for k := range v.messages {
				for _, msg := range v.messages[k] {
					if msg.lineNum == realLineN {
						msgOnLine = true
						gutterStyle := defStyle
						switch msg.kind {
						case GutterInfo:
							if style, ok := colorscheme["gutter-info"]; ok {
								gutterStyle = style
							}
						case GutterWarning:
							if style, ok := colorscheme["gutter-warning"]; ok {
								gutterStyle = style
							}
						case GutterError:
							if style, ok := colorscheme["gutter-error"]; ok {
								gutterStyle = style
							}
						}
						screen.SetContent(screenX, yOffset+visualLineN, '>', nil, gutterStyle)
						screenX++
						screen.SetContent(screenX, yOffset+visualLineN, '>', nil, gutterStyle)
						screenX++
						if v.Cursor.Y == realLineN && !messenger.hasPrompt {
							messenger.Message(msg.msg)
							messenger.gutterMessage = true
						}
					}
				}
			}
			// If there is no message on this line we just display an empty offset
			if !msgOnLine {
				screen.SetContent(screenX, yOffset+visualLineN, ' ', nil, defStyle)
				screenX++
				screen.SetContent(screenX, yOffset+visualLineN, ' ', nil, defStyle)
				screenX++
				if v.Cursor.Y == realLineN && messenger.gutterMessage {
					messenger.Reset()
					messenger.gutterMessage = false
				}
			}
		}

		lineNumStyle := defStyle
		if v.Buf.Settings["ruler"] == true {
			// Write the line number
			if style, ok := colorscheme["line-number"]; ok {
				lineNumStyle = style
			}
			if style, ok := colorscheme["current-line-number"]; ok {
				if realLineN == v.Cursor.Y && tabs[curTab].CurView == v.Num && !v.Cursor.HasSelection() {
					lineNumStyle = style
				}
			}

			lineNum := strconv.Itoa(realLineN + 1)

			// Write the spaces before the line number if necessary
			for i := 0; i < maxLineNumLength-len(lineNum); i++ {
				screen.SetContent(screenX+divider, yOffset+visualLineN, ' ', nil, lineNumStyle)
				screenX++
			}
			if softwrapped && visualLineN != 0 {
				// Pad without the line number because it was written on the visual line before
				for range lineNum {
					screen.SetContent(screenX+divider, yOffset+visualLineN, ' ', nil, lineNumStyle)
					screenX++
				}
			} else {
				// Write the actual line number
				for _, ch := range lineNum {
					screen.SetContent(screenX+divider, yOffset+visualLineN, ch, nil, lineNumStyle)
					screenX++
				}
			}

			// Write the extra space
			screen.SetContent(screenX+divider, yOffset+visualLineN, ' ', nil, lineNumStyle)
			screenX++
		}

		var lastChar *Char
		cursorSet := false
		for _, char := range line {
			if char != nil {
				lineStyle := char.style

				colorcolumn := int(v.Buf.Settings["colorcolumn"].(float64))
				if colorcolumn != 0 && char.visualLoc.X == colorcolumn {
					style := GetColor("color-column")
					fg, _, _ := style.Decompose()
					lineStyle = lineStyle.Background(fg)
				}

				charLoc := char.realLoc
				for _, c := range v.Buf.cursors {
					v.SetCursor(c)
					if v.Cursor.HasSelection() &&
						(charLoc.GreaterEqual(v.Cursor.CurSelection[0]) && charLoc.LessThan(v.Cursor.CurSelection[1]) ||
							charLoc.LessThan(v.Cursor.CurSelection[0]) && charLoc.GreaterEqual(v.Cursor.CurSelection[1])) {
						// The current character is selected
						lineStyle = defStyle.Reverse(true)

						if style, ok := colorscheme["selection"]; ok {
							lineStyle = style
						}
					}
				}
				v.SetCursor(&v.Buf.Cursor)

				if v.Buf.Settings["cursorline"].(bool) && tabs[curTab].CurView == v.Num &&
					!v.Cursor.HasSelection() && v.Cursor.Y == realLineN {
					style := GetColor("cursor-line")
					fg, _, _ := style.Decompose()
					lineStyle = lineStyle.Background(fg)
				}

				screen.SetContent(xOffset+char.visualLoc.X, yOffset+char.visualLoc.Y, char.drawChar, nil, lineStyle)

				for i, c := range v.Buf.cursors {
					v.SetCursor(c)
					if tabs[curTab].CurView == v.Num && !v.Cursor.HasSelection() &&
						v.Cursor.Y == char.realLoc.Y && v.Cursor.X == char.realLoc.X && (!cursorSet || i != 0) {
						ShowMultiCursor(xOffset+char.visualLoc.X, yOffset+char.visualLoc.Y, i)
						cursorSet = true
					}
				}
				v.SetCursor(&v.Buf.Cursor)

				lastChar = char
			}
		}

		lastX := 0
		var realLoc Loc
		var visualLoc Loc
		var cx, cy int
		if lastChar != nil {
			lastX = xOffset + lastChar.visualLoc.X + lastChar.width
			for i, c := range v.Buf.cursors {
				v.SetCursor(c)
				if tabs[curTab].CurView == v.Num && !v.Cursor.HasSelection() &&
					v.Cursor.Y == lastChar.realLoc.Y && v.Cursor.X == lastChar.realLoc.X+1 {
					ShowMultiCursor(lastX, yOffset+lastChar.visualLoc.Y, i)
					cx, cy = lastX, yOffset+lastChar.visualLoc.Y
				}
			}
			v.SetCursor(&v.Buf.Cursor)
			realLoc = Loc{lastChar.realLoc.X + 1, realLineN}
			visualLoc = Loc{lastX - xOffset, lastChar.visualLoc.Y}
		} else if len(line) == 0 {
			for i, c := range v.Buf.cursors {
				v.SetCursor(c)
				if tabs[curTab].CurView == v.Num && !v.Cursor.HasSelection() &&
					v.Cursor.Y == realLineN {
					ShowMultiCursor(xOffset, yOffset+visualLineN, i)
					cx, cy = xOffset, yOffset+visualLineN
				}
			}
			v.SetCursor(&v.Buf.Cursor)
			lastX = xOffset
			realLoc = Loc{0, realLineN}
			visualLoc = Loc{0, visualLineN}
		}

		if v.Cursor.HasSelection() &&
			(realLoc.GreaterEqual(v.Cursor.CurSelection[0]) && realLoc.LessThan(v.Cursor.CurSelection[1]) ||
				realLoc.LessThan(v.Cursor.CurSelection[0]) && realLoc.GreaterEqual(v.Cursor.CurSelection[1])) {
			// The current character is selected
			selectStyle := defStyle.Reverse(true)

			if style, ok := colorscheme["selection"]; ok {
				selectStyle = style
			}
			screen.SetContent(xOffset+visualLoc.X, yOffset+visualLoc.Y, ' ', nil, selectStyle)
		}

		if v.Buf.Settings["cursorline"].(bool) && tabs[curTab].CurView == v.Num &&
			!v.Cursor.HasSelection() && v.Cursor.Y == realLineN {
			for i := lastX; i < xOffset+v.Width-v.lineNumOffset; i++ {
				style := GetColor("cursor-line")
				fg, _, _ := style.Decompose()
				style = style.Background(fg)
				if !(tabs[curTab].CurView == v.Num && !v.Cursor.HasSelection() && i == cx && yOffset+visualLineN == cy) {
					screen.SetContent(i, yOffset+visualLineN, ' ', nil, style)
				}
			}
		}
	}

	if divider != 0 {
		dividerStyle := defStyle
		if style, ok := colorscheme["divider"]; ok {
			dividerStyle = style
		}
		for i := 0; i < v.Height; i++ {
			screen.SetContent(v.x, yOffset+i, '|', nil, dividerStyle.Reverse(true))
		}
	}

	// Draw the autocomplete display on top of everything.
	v.Completer.Display()
}

// ShowMultiCursor will display a cursor at a location
// If i == 0 then the terminal cursor will be used
// Otherwise a fake cursor will be drawn at the position
func ShowMultiCursor(x, y, i int) {
	if i == 0 {
		screen.ShowCursor(x, y)
	} else {
		r, _, _, _ := screen.GetContent(x, y)
		screen.SetContent(x, y, r, nil, defStyle.Reverse(true))
	}
}

// Display renders the view, the cursor, and statusline
func (v *View) Display() {
	if globalSettings["termtitle"].(bool) {
		screen.SetTitle("micro: " + v.Buf.GetName())
	}
	v.DisplayView()
	// Don't draw the cursor if it is out of the viewport or if it has a selection
	if v.Num == tabs[curTab].CurView && (v.Cursor.Y-v.Topline < 0 || v.Cursor.Y-v.Topline > v.Height-1 || v.Cursor.HasSelection()) {
		screen.HideCursor()
	}
	_, screenH := screen.Size()

	if v.Buf.Settings["scrollbar"].(bool) {
		v.scrollbar.Display()
	}

	if v.Buf.Settings["statusline"].(bool) {
		v.sline.Display()
	} else if (v.y + v.Height) != screenH-1 {
		for x := 0; x < v.Width; x++ {
			screen.SetContent(v.x+x, v.y+v.Height, '-', nil, defStyle.Reverse(true))
		}
	}
}
