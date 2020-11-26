package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/hpcloud/tail"
	flag "github.com/spf13/pflag"
)

var knownPlayers = map[string]string{
	"plasma-browser-integration": "Browser",
	"noson":                      "Noson",
}

var knownBrowsers = map[string]string{
	"mozilla":  "Firefox",
	"chrome":   "Chrome",
	"chromium": "Chromium",
}

type Player struct {
	player                               dbus.BusObject
	fullName, name, title, artist, album string
	position                             int64
	pid                                  uint32
	playing, stopped                     bool
	metadata                             map[string]dbus.Variant
	conn                                 *dbus.Conn
}

const (
	INTERFACE = "org.mpris.MediaPlayer2"
	PATH      = "/org/mpris/MediaPlayer2"
	// NameOwnerChanged
	MATCH_NOC = "type='signal',path='/org/freedesktop/DBus',interface='org.freedesktop.DBus',member='NameOwnerChanged'"
	// PropertiesChanged
	MATCH_PC = "type='signal',path='/org/mpris/MediaPlayer2',interface='org.freedesktop.DBus.Properties'"
	SOCK     = "/tmp/waybar-mpris.sock"
	LOGFILE  = "/tmp/waybar-mpris.log"
	OUTFILE  = "/tmp/waybar-mpris.out"
	POLL     = 1
)

var (
	PLAY                  = "▶"
	PAUSE                 = ""
	SEP                   = " - "
	ORDER                 = "SYMBOL:ARTIST:ALBUM:TITLE:POSITION"
	AUTOFOCUS             = false
	COMMANDS              = []string{"player-next", "player-prev", "next", "prev", "toggle", "list"}
	SHOW_POS              = false
	INTERPOLATE           = false
	REPLACE               = false
	WRITER      io.Writer = os.Stdout
)

// NewPlayer returns a new player object.
func NewPlayer(conn *dbus.Conn, name string) (p *Player) {
	playerName := strings.ReplaceAll(name, INTERFACE+".", "")
	var pid uint32
	conn.BusObject().Call("org.freedesktop.DBus.GetConnectionUnixProcessID", 0, name).Store(&pid)
	for key, val := range knownPlayers {
		if strings.Contains(name, key) {
			playerName = val
			break
		}
	}
	if playerName == "Browser" {
		file, err := ioutil.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err == nil {
			cmd := string(file)
			for key, val := range knownBrowsers {
				if strings.Contains(cmd, key) {
					playerName = val
					break
				}
			}
		}
	}
	p = &Player{
		player:   conn.Object(name, PATH),
		conn:     conn,
		name:     playerName,
		fullName: name,
		pid:      pid,
	}
	p.Refresh()
	return
}

// Refresh grabs playback info.
func (p *Player) Refresh() (err error) {
	val, err := p.player.GetProperty(INTERFACE + ".Player.PlaybackStatus")
	if err != nil {
		p.playing = false
		p.stopped = false
		p.metadata = map[string]dbus.Variant{}
		p.title = ""
		p.artist = ""
		p.album = ""
		return
	}
	strVal := val.String()
	if strings.Contains(strVal, "Playing") {
		p.playing = true
		p.stopped = false
	} else if strings.Contains(strVal, "Paused") {
		p.playing = false
		p.stopped = false
	} else {
		p.playing = false
		p.stopped = true
	}
	metadata, err := p.player.GetProperty(INTERFACE + ".Player.Metadata")
	if err != nil {
		p.metadata = map[string]dbus.Variant{}
		p.title = ""
		p.artist = ""
		p.album = ""
		return
	}
	p.metadata = metadata.Value().(map[string]dbus.Variant)
	switch artist := p.metadata["xesam:artist"].Value().(type) {
	case []string:
		p.artist = strings.Join(artist, ", ")
	case string:
		p.artist = artist
	default:
		p.artist = ""
	}
	switch title := p.metadata["xesam:title"].Value().(type) {
	case string:
		p.title = title
	default:
		p.title = ""
	}
	switch album := p.metadata["xesam:album"].Value().(type) {
	case string:
		p.album = album
	default:
		p.album = ""
	}
	return nil
}

func µsToString(µs int64) string {
	seconds := int(µs / 1e6)
	minutes := int(seconds / 60)
	seconds -= minutes * 60
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}

// inc is the increment in seconds to add if the player reports an identical position.
func (p *Player) Position() string {
	// position is in microseconds so we prob need int64 to be safe
	v := p.metadata["mpris:length"].Value()
	var l int64
	if v != nil {
		l = v.(int64)
	} else {
		return ""
	}
	length := µsToString(l)
	if length == "" {
		return ""
	}
	pos, err := p.player.GetProperty(INTERFACE + ".Player.Position")
	if err != nil {
		return ""
	}
	position := µsToString(pos.Value().(int64))
	if position == "" {
		return ""
	}
	if INTERPOLATE && position == µsToString(p.position) {
		np := p.position + int64(POLL*1000000)
		position = µsToString(np)
	}
	p.position = pos.Value().(int64)
	return position + "/" + length
}

func (p *Player) JSON() string {
	data := map[string]string{}
	symbol := PLAY
	data["class"] = "paused"
	if p.playing {
		symbol = PAUSE
		data["class"] = "playing"
	}
	var pos string
	if SHOW_POS {
		pos = p.Position()
		if pos != "" {
			pos = "(" + pos + ")"
		}
	}
	var items []string
	order := strings.Split(ORDER, ":")
	for _, v := range order {
		if v == "SYMBOL" {
			items = append(items, symbol)
		} else if v == "ARTIST" {
			if p.artist != "" {
				items = append(items, p.artist)
			}
		} else if v == "ALBUM" {
			if p.album != "" {
				items = append(items, p.album)
			}
		} else if v == "TITLE" {
			if p.title != "" {
				items = append(items, p.title)
			}
		} else if v == "POSITION" && SHOW_POS {
			if pos != "" {
				items = append(items, pos)
			}
		}
	}
	if len(items) == 0 {
		return "{}"
	}
	text := ""
	for i, v := range items {
		right := ""
		if (v == symbol || v == pos) && i != len(items)-1 {
			right = " "
		} else if i != len(items)-1 && items[i+1] != symbol && items[i+1] != pos {
			right = SEP
		} else {
			right = " "
		}
		text += v + right
	}

	data["tooltip"] = fmt.Sprintf(
		"%s\nby %s\n",
		strings.ReplaceAll(p.title, "&", "&amp;"),
		strings.ReplaceAll(p.artist, "&", "&amp;"))
	if p.album != "" {
		data["tooltip"] += "from " + strings.ReplaceAll(p.album, "&", "&amp;") + "\n"
	}
	data["tooltip"] += "(" + p.name + ")"
	data["text"] = text
	out, err := json.Marshal(data)
	if err != nil {
		return "{}"
	}
	return string(out)
}

type PlayerList struct {
	list    List
	current uint
	conn    *dbus.Conn
}

type List []*Player

func (ls List) Len() int {
	return len(ls)
}

func (ls List) Less(i, j int) bool {
	var states [2]uint8
	for i, p := range []bool{ls[i].playing, ls[j].playing} {
		if p {
			states[i] = 1
		}
	}
	// Reverse order
	return states[0] > states[1]
}

func (ls List) Swap(i, j int) {
	ls[i], ls[j] = ls[j], ls[i]
}

func (pl *PlayerList) Remove(fullName string) {
	currentName := pl.list[pl.current].fullName
	var i int
	found := false
	for ind, p := range pl.list {
		if p.fullName == fullName {
			i = ind
			found = true
			break
		}
	}
	if found {
		pl.list[0], pl.list[i] = pl.list[i], pl.list[0]
		pl.list = pl.list[1:]
		found = false
		for ind, p := range pl.list {
			if p.fullName == currentName {
				pl.current = uint(ind)
				found = true
				break
			}
		}
		if !found {
			pl.current = 0
			pl.Refresh()
			fmt.Fprintln(WRITER, pl.JSON())
		}
	}
	// ls[len(ls)-1], ls[i] = ls[i], ls[len(ls)-1]
	// ls = ls[:len(ls)-1]
}

func (pl *PlayerList) Reload() error {
	var buses []string
	err := pl.conn.BusObject().Call("org.freedesktop.DBus.ListNames", 0).Store(&buses)
	if err != nil {
		return err
	}
	for _, name := range buses {
		// Don't add playerctld, it just duplicates other players
		if strings.HasPrefix(name, INTERFACE) && !strings.Contains(name, "playerctld") {
			pl.New(name)
		}
	}
	return nil
}

func (pl *PlayerList) New(name string) {
	pl.list = append(pl.list, NewPlayer(pl.conn, name))
	if AUTOFOCUS {
		pl.current = uint(len(pl.list) - 1)
	}
}

func (pl *PlayerList) Sort() {
	sort.Sort(pl.list)
	pl.current = 0
}

func (pl *PlayerList) Refresh() {
	for i := range pl.list {
		pl.list[i].Refresh()
	}
}

func (pl *PlayerList) JSON() string {
	if len(pl.list) != 0 {
		return pl.list[pl.current].JSON()
	}
	return "{}"
}

func (pl *PlayerList) Next() {
	pl.list[pl.current].player.Call(INTERFACE+".Player.Next", 0)
}

func (pl *PlayerList) Prev() {
	pl.list[pl.current].player.Call(INTERFACE+".Player.Previous", 0)
}

func (pl *PlayerList) Toggle() {
	pl.list[pl.current].player.Call(INTERFACE+".Player.PlayPause", 0)
}

func main() {
	logfile, err := os.OpenFile(LOGFILE, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0666)
	if err != nil {
		log.Fatalf("Couldn't open %s for writing: %s", LOGFILE, err)
	}
	mw := io.MultiWriter(logfile, os.Stdout)
	log.SetOutput(mw)
	flag.StringVar(&PLAY, "play", PLAY, "Play symbol/text to use.")
	flag.StringVar(&PAUSE, "pause", PAUSE, "Pause symbol/text to use.")
	flag.StringVar(&SEP, "separator", SEP, "Separator string to use between artist, album, and title.")
	flag.StringVar(&ORDER, "order", ORDER, "Element order.")
	flag.BoolVar(&AUTOFOCUS, "autofocus", AUTOFOCUS, "Auto switch to currently playing music players.")
	flag.BoolVar(&SHOW_POS, "position", SHOW_POS, "Show current position between brackets, e.g (04:50/05:00)")
	flag.BoolVar(&INTERPOLATE, "interpolate", INTERPOLATE, "Interpolate track position (helpful for players that don't update regularly, e.g mpDris2)")
	flag.BoolVar(&REPLACE, "replace", REPLACE, "replace existing waybar-mpris if found. When false, new instance will clone the original instances output.")
	var command string
	flag.StringVar(&command, "send", "", "send command to already runnning waybar-mpris instance. (options: "+strings.Join(COMMANDS, "/")+")")

	flag.Parse()
	os.Stderr = logfile

	if command != "" {
		conn, err := net.Dial("unix", SOCK)
		if err != nil {
			log.Fatalln("Couldn't dial:", err)
		}
		_, err = conn.Write([]byte(command))
		if err != nil {
			log.Fatalln("Couldn't send command")
		}
		fmt.Println("Sent.")
		if command == "list" {
			buf := make([]byte, 512)
			nr, err := conn.Read(buf)
			if err != nil {
				log.Fatalln("Couldn't read response.")
			}
			response := string(buf[0:nr])
			fmt.Println("Response:")
			fmt.Printf(response)
		}
		os.Exit(0)
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		log.Fatalln("Error connecting to DBus:", err)
	}
	players := &PlayerList{
		conn: conn,
	}
	players.Reload()
	players.Sort()
	players.Refresh()
	fmt.Fprintln(WRITER, players.JSON())
	lastLine := ""
	// fmt.Println("New array", players)
	// Start command listener
	if _, err := os.Stat(SOCK); err == nil {
		if REPLACE {
			fmt.Printf("Socket %s already exists, this could mean waybar-mpris is already running.\nStarting this instance will overwrite the file, possibly stopping other instances from accepting commands.\n", SOCK)
			var input string
			ignoreChoice := false
			fmt.Printf("Continue? [y/n]: ")
			go func() {
				fmt.Scanln(&input)
				if strings.Contains(input, "y") && !ignoreChoice {
					os.Remove(SOCK)
				}
			}()
			time.Sleep(5 * time.Second)
			if input == "" {
				fmt.Printf("\nRemoving due to lack of input.\n")
				ignoreChoice = true
				// os.Remove(SOCK)
			}
		} else if conn, err := net.Dial("unix", SOCK); err == nil {
			// When waybar-mpris is already running, we attach to its output instead of launching a whole new instance.
			// Print to stderr to avoid errors from waybar
			os.Stderr.WriteString("waybar-mpris is already running. This instance will clone its output.")
			if err != nil {
				log.Fatalln("Couldn't dial:", err)
			}
			_, err = conn.Write([]byte("share"))
			if err != nil {
				log.Fatalln("Couldn't send command")
			}
			buf := make([]byte, 512)
			nr, err := conn.Read(buf)
			if err != nil {
				log.Fatalln("Couldn't read response.")
			}
			if string(buf[0:nr]) == "success" {
				t, err := tail.TailFile(OUTFILE, tail.Config{
					Follow:    true,
					MustExist: true,
					Logger:    tail.DiscardingLogger,
				})
				if err == nil {
					for line := range t.Lines {
						fmt.Println(line.Text)
					}
				}
			}
		} else {
			os.Remove(SOCK)
			os.Remove(OUTFILE)
		}
	}
	go func() {
		listener, err := net.Listen("unix", SOCK)
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		go func() {
			<-c
			os.Remove(OUTFILE)
			os.Remove(SOCK)
			os.Exit(1)
		}()
		if err != nil {
			log.Fatalf("Couldn't establish socket connection at %s (error %s)\n", SOCK, err)
		}
		defer func() {
			listener.Close()
			os.Remove(SOCK)
		}()
		for {
			con, err := listener.Accept()
			if err != nil {
				log.Println("Couldn't accept:", err)
				continue
			}
			buf := make([]byte, 512)
			nr, err := con.Read(buf)
			if err != nil {
				log.Println("Couldn't read:", err)
				continue
			}
			command := string(buf[0:nr])
			switch command {
			case "player-next":
				length := len(players.list)
				if length != 1 {
					if players.current < uint(length-1) {
						players.current += 1
					} else {
						players.current = 0
					}
					players.Refresh()
					fmt.Fprintln(WRITER, players.JSON())
				}
			case "player-prev":
				length := len(players.list)
				if length != 1 {
					if players.current != 0 {
						players.current -= 1
					} else {
						players.current = uint(length - 1)
					}
					players.Refresh()
					fmt.Fprintln(WRITER, players.JSON())
				}
			case "next":
				players.Next()
			case "prev":
				players.Prev()
			case "toggle":
				players.Toggle()
			case "list":
				resp := ""
				pad := 0
				i := len(players.list)
				for i != 0 {
					i /= 10
					pad++
				}
				for i, p := range players.list {
					symbol := ""
					if uint(i) == players.current {
						symbol = "*"
					}
					resp += fmt.Sprintf("%0"+strconv.Itoa(pad)+"d%s: Name: %s; Playing: %t; PID: %d\n", i, symbol, p.fullName, p.playing, p.pid)
				}
				con.Write([]byte(resp))
			case "share":
				out, err := os.Create(OUTFILE)
				if err != nil {
					con.Write([]byte(fmt.Sprintf("Failed: %s", err)))
				}
				WRITER = io.MultiWriter(os.Stdout, out)
				con.Write([]byte("success"))
			default:
				fmt.Println("Invalid command")
			}
		}
	}()

	conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, MATCH_NOC)
	conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, MATCH_PC)
	if SHOW_POS {
		go func() {
			for {
				time.Sleep(POLL * time.Second)
				if len(players.list) != 0 {
					if players.list[players.current].playing {
						go fmt.Fprintln(WRITER, players.JSON())
					}
				}
			}
		}()
	}
	c := make(chan *dbus.Signal, 10)
	conn.Signal(c)
	for v := range c {
		// fmt.Printf("SIGNAL: Sender %s, Path %s, Name %s, Body %s\n", v.Sender, v.Path, v.Name, v.Body)
		if strings.Contains(v.Name, "NameOwnerChanged") {
			switch name := v.Body[0].(type) {
			case string:
				var pid uint32
				conn.BusObject().Call("org.freedesktop.DBus.GetConnectionUnixProcessID", 0, name).Store(&pid)
				// Ignore playerctld again
				if strings.Contains(name, INTERFACE) && !strings.Contains(name, "playerctld") {
					if pid == 0 {
						// fmt.Println("Removing", name)
						players.Remove(name)
					} else {
						// fmt.Println("Adding", name)
						players.New(name)
					}
				}
			}
		} else if strings.Contains(v.Name, "PropertiesChanged") && strings.Contains(v.Body[0].(string), INTERFACE+".Player") {
			players.Refresh()
			if AUTOFOCUS {
				players.Sort()
			}
			if l := players.JSON(); l != lastLine {
				lastLine = l
				fmt.Fprintln(WRITER, l)
			}
		}
	}
}
