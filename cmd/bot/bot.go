package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/bwmarrin/discordgo"
	"github.com/layeh/gopus"
	redis "gopkg.in/redis.v3"
)

var (
	// discordgo session
	discord *discordgo.Session

	// Redis client connection (used for stats)
	rcli *redis.Client

	// Map of Guild id's to *Play channels, used for queuing and rate-limiting guilds
	queues map[string]chan *Play = make(map[string]chan *Play)

	// Sound attributes
	AIRHORN_SOUND_RANGE = 0
	KHALED_SOUND_RANGE  = 0
	BITRATE             = 128
	MAX_QUEUE_SIZE      = 6

	// Sound Types
	TYPE_AIRHORN = 0
	TYPE_KHALED  = 1
)

// Play represents an individual use of the !airhorn command
type Play struct {
	GuildID   string
	ChannelID string
	UserID    string
	Sound     *Sound

	// If true, this was a forced play using a specific airhorn sound name
	Forced bool

	// If true, we need to appreciate this value
	Khaled bool
}

// Sound represents a sound clip
type Sound struct {
	Name string

	// Weight adjust how likely it is this song will play, higher = more likely
	Weight int

	// Delay (in milliseconds) for the bot to wait before sending the disconnect request
	PartDelay int

	// Sound Type
	Type int

	// Channel used for the encoder routine
	encodeChan chan []int16

	// Buffer to store encoded PCM packets
	buffer [][]byte
}

func createSound(Name string, Weight int, PartDelay int, Type int) *Sound {
	return &Sound{
		Name:       Name,
		Weight:     Weight,
		PartDelay:  PartDelay,
		Type:       Type,
		encodeChan: make(chan []int16, 10),
		buffer:     make([][]byte, 0),
	}
}

// Array of all the sounds we have
var AIRHORNS []*Sound = []*Sound{
	createSound("default", 1000, 250, TYPE_AIRHORN),
	createSound("reverb", 800, 250, TYPE_AIRHORN),
	createSound("spam", 800, 0, TYPE_AIRHORN),
	createSound("tripletap", 800, 250, TYPE_AIRHORN),
	createSound("fourtap", 800, 250, TYPE_AIRHORN),
	createSound("distant", 500, 250, TYPE_AIRHORN),
	createSound("echo", 500, 250, TYPE_AIRHORN),
	createSound("clownfull", 250, 250, TYPE_AIRHORN),
	createSound("clownshort", 250, 250, TYPE_AIRHORN),
	createSound("clownspam", 250, 0, TYPE_AIRHORN),
	createSound("horn_highfartlong", 200, 250, TYPE_AIRHORN),
	createSound("horn_highfartshort", 200, 250, TYPE_AIRHORN),
	createSound("midshort", 100, 250, TYPE_AIRHORN),
	createSound("truck", 10, 250, TYPE_AIRHORN),
}

var KHALED []*Sound = []*Sound{
	createSound("one", 1, 250, TYPE_KHALED),
	createSound("one_classic", 1, 250, TYPE_KHALED),
	createSound("one_echo", 1, 250, TYPE_KHALED),
}

// Encode reads data from ffmpeg and encodes it using gopus
func (s *Sound) Encode() {
	encoder, err := gopus.NewEncoder(48000, 2, gopus.Audio)
	if err != nil {
		fmt.Println("NewEncoder Error:", err)
		return
	}

	encoder.SetBitrate(BITRATE * 1000)
	encoder.SetApplication(gopus.Audio)

	for {
		pcm, ok := <-s.encodeChan
		if !ok {
			// if chan closed, exit
			return
		}

		// try encoding pcm frame with Opus
		opus, err := encoder.Encode(pcm, 960, 960*2*2)
		if err != nil {
			fmt.Println("Encoding Error:", err)
			return
		}

		// Append the PCM frame to our buffer
		s.buffer = append(s.buffer, opus)
	}
}

// Load attempts to load and encode a sound file from disk
func (s *Sound) Load() error {
	s.encodeChan = make(chan []int16, 10)
	defer close(s.encodeChan)
	go s.Encode()

	var path string
	if s.Type == TYPE_AIRHORN {
		path = fmt.Sprintf("audio/airhorn_%v.wav", s.Name)
	} else {
		path = fmt.Sprintf("audio/another_%v.wav", s.Name)
	}

	ffmpeg := exec.Command("ffmpeg", "-i", path, "-f", "s16le", "-ar", "48000", "-ac", "2", "pipe:1")
	stdout, err := ffmpeg.StdoutPipe()
	if err != nil {
		fmt.Println("StdoutPipe Error:", err)
		return err
	}

	err = ffmpeg.Start()
	if err != nil {
		fmt.Println("RunStart Error:", err)
		return err
	}

	for {
		// read data from ffmpeg stdout
		InBuf := make([]int16, 960*2)
		err = binary.Read(stdout, binary.LittleEndian, &InBuf)

		// If this is the end of the file, just return
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}

		if err != nil {
			fmt.Println("error reading from ffmpeg stdout :", err)
			return err
		}

		// write pcm data to the encodeChan
		s.encodeChan <- InBuf
	}
}

// Plays this sound over the specified VoiceConnection
func (s *Sound) Play(vc *discordgo.VoiceConnection) {
	vc.Speaking(true)
	defer vc.Speaking(false)

	for _, buff := range s.buffer {
		vc.OpusSend <- buff
	}
}

// Attempts to find the current users voice channel inside a given guild
func getCurrentVoiceChannel(user *discordgo.User, guild *discordgo.Guild) *discordgo.Channel {
	for _, vs := range guild.VoiceStates {
		if vs.UserID == user.ID {
			channel, _ := discord.State.Channel(vs.ChannelID)
			return channel
		}
	}
	return nil
}

// Returns a random integer between min and max
func randomRange(min, max int) int {
	rand.Seed(time.Now().Unix())
	return rand.Intn(max-min) + min
}

// Returns a random sound
func getRandomSound(stype int) *Sound {
	var i int

	if stype == TYPE_AIRHORN {
		number := randomRange(0, AIRHORN_SOUND_RANGE)

		for _, item := range AIRHORNS {
			i += item.Weight

			if number < i {
				return item
			}
		}
	} else {
		number := randomRange(0, KHALED_SOUND_RANGE)

		for _, item := range KHALED {
			i += item.Weight

			if number < i {
				return item
			}
		}

	}

	return nil
}

// Enqueues a play into the ratelimit/buffer guild queue
func enqueuePlay(user *discordgo.User, guild *discordgo.Guild, sound *Sound, isKhaled bool) {
	// Grab the users voice channel
	channel := getCurrentVoiceChannel(user, guild)
	if channel == nil {
		log.WithFields(log.Fields{
			"user":  user.ID,
			"guild": guild.ID,
		}).Warning("Failed to find channel to play sound in")
		return
	}

	var forced bool = true
	if sound == nil {
		forced = false
		sound = getRandomSound(TYPE_AIRHORN)
	}

	play := &Play{
		GuildID:   guild.ID,
		ChannelID: channel.ID,
		Sound:     sound,
		Forced:    forced,
		Khaled:    isKhaled,
	}

	// Check if we already have a connection to this guild
	_, exists := queues[guild.ID]

	if exists {
		if len(queues[guild.ID]) < MAX_QUEUE_SIZE {
			queues[guild.ID] <- play
		}
	} else {
		queues[guild.ID] = make(chan *Play, MAX_QUEUE_SIZE)
		playSound(play, nil)
	}
}

func trackSoundStats(play *Play) {
	if rcli == nil {
		return
	}

	_, err := rcli.Pipelined(func(pipe *redis.Pipeline) error {
		var baseChar string

		if play.Forced {
			baseChar = "f"
		} else {
			baseChar = "a"
		}

		base := fmt.Sprintf("airhorn:%s", baseChar)

		pipe.Incr("airhorn:total")
		pipe.Incr(fmt.Sprintf("%s:total", base))
		pipe.Incr(fmt.Sprintf("%s:sound:%s", base, play.Sound.Name))
		pipe.Incr(fmt.Sprintf("%s:user:%s:sound:%s", base, play.UserID, play.Sound.Name))
		pipe.Incr(fmt.Sprintf("%s:guild:%s:sound:%s", base, play.GuildID, play.Sound.Name))
		pipe.Incr(fmt.Sprintf("%s:guild:%s:chan:%s:sound:%s", base, play.GuildID, play.ChannelID, play.Sound.Name))
		pipe.SAdd(fmt.Sprintf("%s:users", base), play.UserID)
		pipe.SAdd(fmt.Sprintf("%s:guilds", base), play.GuildID)
		pipe.SAdd(fmt.Sprintf("%s:channels", base), play.ChannelID)
		return nil
	})

	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Warning("Failed to track stats in redis")
	}
}

// Play a sound
func playSound(play *Play, vc *discordgo.VoiceConnection) (err error) {
	log.WithFields(log.Fields{
		"play": play,
	}).Info("Playing sound")

	if vc == nil {
		vc, err = discord.ChannelVoiceJoin(play.GuildID, play.ChannelID, false, false)
		// vc.Receive = false
		if err != nil {
			log.WithFields(log.Fields{
				"error": err,
			}).Error("Failed to play sound")
			delete(queues, play.GuildID)
			return err
		}

		/*
			err = vc.WaitUntilConnected()
			if err != nil {
				log.WithFields(log.Fields{
					"error": err,
					"play":  play,
				}).Error("Failed to join voice channel")
				vc.Close()
				delete(queues, play.GuildID)
				return err
			}
		*/
	}

	if vc.ChannelID != play.ChannelID {
		vc.ChangeChannel(play.ChannelID, false, false)
		time.Sleep(time.Millisecond * 200)
	}

	// Sleep for a specified amount of time before playing the sound
	time.Sleep(time.Millisecond * 32)

	// If we're appreciating this sound, lets play some DJ KHALLLLLEEEEDDDD
	if play.Khaled {
		log.Info("KHALED")
		dj := getRandomSound(TYPE_KHALED)
		dj.Play(vc)
	}

	// Track stats for this play in redis
	go trackSoundStats(play)

	// Play the sound
	play.Sound.Play(vc)

	// If there is another song in the queue, recurse and play that
	if len(queues[play.GuildID]) > 0 {
		play := <-queues[play.GuildID]
		playSound(play, vc)
		return nil
	}

	// If the queue is empty, delete it
	time.Sleep(time.Millisecond * time.Duration(play.Sound.PartDelay))
	delete(queues, play.GuildID)
	vc.Disconnect()
	return nil
}

func onGuildCreate(s *discordgo.Session, event *discordgo.GuildCreate) {
	for _, channel := range event.Guild.Channels {
		if channel.ID == event.Guild.ID {
			s.ChannelMessageSend(channel.ID, "**AIRHORN BOT READY FOR HORNING. TYPE `!AIRHORN` IN CHAT TO ACTIVATE**")
			return
		}
	}
}

func onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	parts := strings.Split(strings.ToLower(m.Content), " ")
	var (
		sound  *Sound
		khaled bool
	)

	if parts[0] == "!airhorn" || parts[0] == "!anotha" || parts[0] == "!anoathaone" {
		channel, _ := discord.State.Channel(m.ChannelID)
		if channel == nil {
			log.WithFields(log.Fields{
				"channel": m.ChannelID,
				"message": m.ID,
			}).Warning("Failed to grab channel")
			return
		}

		guild, _ := discord.State.Guild(channel.GuildID)
		if guild == nil {
			log.WithFields(log.Fields{
				"guild":   channel.GuildID,
				"channel": channel,
				"message": m.ID,
			}).Warning("Failed to grab guild")
			return
		}

		// Support !airhorn <sound>
		if len(parts) > 1 {
			for _, s := range AIRHORNS {
				if parts[1] == s.Name {
					sound = s
				}
			}

			if sound == nil {
				return
			}
		}

		if strings.HasPrefix(parts[0], "!anotha") {
			khaled = true
		}

		go enqueuePlay(m.Author, guild, sound, khaled)
	}
}

func main() {
	var (
		Token = flag.String("t", "", "Discord Authentication Token")
		Redis = flag.String("r", "", "Redis Connection String")
		err   error
	)
	flag.Parse()

	// Preload all the sounds
	log.Info("Preloading sounds...")
	for _, sound := range AIRHORNS {
		AIRHORN_SOUND_RANGE += sound.Weight
		sound.Load()
	}

	log.Info("Preloading loyalty...")
	for _, sound := range KHALED {
		KHALED_SOUND_RANGE += sound.Weight
		sound.Load()
	}

	// If we got passed a redis server, try to connect
	if *Redis != "" {
		log.Info("Connecting to redis...")
		rcli = redis.NewClient(&redis.Options{Addr: *Redis, DB: 0})
		_, err = rcli.Ping().Result()

		if err != nil {
			log.WithFields(log.Fields{
				"error": err,
			}).Fatal("Failed to connect to redis")
			return
		}
	}

	// Create a discord session
	log.Info("Starting discord session...")
	discord, err = discordgo.New(*Token)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Fatal("Failed to create discord session")
		return
	}

	discord.AddHandler(onGuildCreate)
	discord.AddHandler(onMessageCreate)

	err = discord.Open()
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Fatal("Failed to create discord websocket connection")
		return
	}

	// We're running!
	log.Info("AIRHORNBOT is ready to horn it up.")

	// Wait for a signal to quit
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill)
	<-c
}