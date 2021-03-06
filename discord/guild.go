package discord

import (
	"fmt"
	"github.com/bwmarrin/discordgo"
	"github.com/denverquane/amongusdiscord/game"
	"log"
	"sync"
)

// GameDelays struct
type GameDelays struct {
	//maps from origin->new phases, with the integer number of seconds for the delay
	delays map[game.Phase]map[game.Phase]int
	lock   sync.RWMutex
}

func MakeDefaultDelays() GameDelays {
	return GameDelays{
		delays: map[game.Phase]map[game.Phase]int{
			game.LOBBY: {
				game.LOBBY:   0,
				game.TASKS:   7,
				game.DISCUSS: 0,
			},
			game.TASKS: {
				game.LOBBY:   1,
				game.TASKS:   0,
				game.DISCUSS: 0,
			},
			game.DISCUSS: {
				game.LOBBY:   6,
				game.TASKS:   7,
				game.DISCUSS: 0,
			},
		},
		lock: sync.RWMutex{},
	}
}

func (gd *GameDelays) GetDelay(origin, dest game.Phase) int {
	gd.lock.RLock()
	defer gd.lock.RUnlock()
	return gd.delays[origin][dest]
}

// GuildState struct
type GuildState struct {
	ID string

	LinkCode string

	UserData UserDataSet
	Tracking Tracking
	//use this to refer to the same state message and update it on ls
	GameStateMsg GameStateMessage

	Delays        GameDelays
	StatusEmojis  AlivenessEmojis
	SpecialEmojis map[string]Emoji

	AmongUsData game.AmongUsData

	VoiceRules VoiceRules

	//if the users should be nick-named using the in-game names
	ApplyNicknames bool
	CommandPrefix  string
}

// TrackedMemberAction struct
type TrackedMemberAction struct {
	mute          bool
	move          bool
	message       string
	targetChannel Tracking
}

func (guild *GuildState) addFullUserToMap(g *discordgo.Guild, userID string) {
	for _, v := range g.Members {
		if v.User.ID == userID {
			guild.UserData.AddFullUser(game.MakeUserDataFromDiscordUser(v.User, v.Nick))
			return
		}
	}
	guild.UserData.AddFullUser(game.MakeMinimalUserData(userID))
}

//handleTrackedMembers moves/mutes players according to the current game state
func (guild *GuildState) handleTrackedMembers(dg *discordgo.Session, delay int) {

	g := guild.verifyVoiceStateChanges(dg)

	updateMade := false
	for _, voiceState := range g.VoiceStates {

		userData, err := guild.UserData.GetUser(voiceState.UserID)
		if err == nil {
			tracked := guild.Tracking.IsTracked(voiceState.ChannelID)
			//only actually tracked if we're in a tracked channel AND linked to a player
			tracked = tracked && userData.IsLinked()
			shouldMute, shouldDeaf := guild.VoiceRules.GetVoiceState(userData.IsAlive(), tracked, guild.AmongUsData.GetPhase())

			nick := userData.GetPlayerName()
			if !guild.ApplyNicknames {
				nick = ""
			}

			//only issue a change if the user isn't in the right state already
			//nicksmatch can only be false if the in-game data is != nil, so the reference to .audata below is safe
			if shouldMute != voiceState.Mute || shouldDeaf != voiceState.Deaf || (nick != "" && userData.GetNickName() != userData.GetPlayerName()) {

				//only issue the req to discord if we're not waiting on another one
				if !userData.IsPendingVoiceUpdate() {
					//wait until it goes through
					userData.SetPendingVoiceUpdate(true)

					guild.UserData.UpdateUserData(voiceState.UserID, userData)

					go guildMemberUpdate(dg, guild.ID, voiceState.UserID, UserPatchParameters{shouldDeaf, shouldMute, nick}, delay)

					updateMade = true
				}

			} else {
				if shouldMute {
					log.Printf("Not muting %s because they're already muted\n", userData.GetUserName())
				} else {
					log.Printf("Not unmuting %s because they're already unmuted\n", userData.GetUserName())
				}
			}
		} else { //the user doesn't exist in our userdata cache; add them
			guild.addFullUserToMap(g, voiceState.UserID)
		}
	}
	if updateMade {
		log.Println("Updating state message")
		guild.GameStateMsg.Edit(dg, gameStateResponse(guild))
	}
}

func (guild *GuildState) verifyVoiceStateChanges(s *discordgo.Session) *discordgo.Guild {
	g, err := s.State.Guild(guild.ID)
	if err != nil {
		log.Println(err)
	}

	for _, voiceState := range g.VoiceStates {
		userData, err := guild.UserData.GetUser(voiceState.UserID)
		if err == nil {
			tracked := guild.Tracking.IsTracked(voiceState.ChannelID)
			//only actually tracked if we're in a tracked channel AND linked to a player
			tracked = tracked && userData.IsLinked()
			mute, deaf := guild.VoiceRules.GetVoiceState(userData.IsAlive(), tracked, guild.AmongUsData.GetPhase())
			if userData.IsPendingVoiceUpdate() && voiceState.Mute == mute && voiceState.Deaf == deaf {
				userData.SetPendingVoiceUpdate(false)

				guild.UserData.UpdateUserData(voiceState.UserID, userData)

				//log.Println("Successfully updated pendingVoice")
			}
		} else { //the user doesn't exist in our userdata cache; add them
			guild.addFullUserToMap(g, voiceState.UserID)
		}
	}
	return g

}

//voiceStateChange handles more edge-case behavior for users moving between voice channels, and catches when
//relevant discord api requests are fully applied successfully. Otherwise, we can issue multiple requests for
//the same mute/unmute, erroneously
func (guild *GuildState) voiceStateChange(s *discordgo.Session, m *discordgo.VoiceStateUpdate) {
	g := guild.verifyVoiceStateChanges(s)

	updateMade := false

	//fetch the userData from our userData data cache
	userData, err := guild.UserData.GetUser(m.UserID)
	if err == nil {
		tracked := guild.Tracking.IsTracked(m.ChannelID)
		//only actually tracked if we're in a tracked channel AND linked to a player
		tracked = tracked && userData.IsLinked()
		mute, deaf := guild.VoiceRules.GetVoiceState(userData.IsAlive(), tracked, guild.AmongUsData.GetPhase())
		if !userData.IsPendingVoiceUpdate() && (mute != m.Mute || deaf != m.Deaf) {
			userData.SetPendingVoiceUpdate(true)

			guild.UserData.UpdateUserData(m.UserID, userData)

			nick := userData.GetPlayerName()
			if !guild.ApplyNicknames {
				nick = ""
			}

			go guildMemberUpdate(s, m.GuildID, m.UserID, UserPatchParameters{deaf, mute, nick}, 0)

			log.Println("Applied deaf/undeaf mute/unmute via voiceStateChange")

			updateMade = true
		}
	} else { //the userData doesn't exist in our userdata cache; add them
		guild.addFullUserToMap(g, m.UserID)
	}

	if updateMade {
		log.Println("Updating state message")
		guild.GameStateMsg.Edit(s, gameStateResponse(guild))
	}
}

// TODO this probably deals with too much direct state-changing;
//probably want to bubble it up to some higher authority?
func (guild *GuildState) handleReactionGameStartAdd(s *discordgo.Session, m *discordgo.MessageReactionAdd) {
	//g, err := s.State.Guild(guild.ID)
	//if err != nil {
	//	log.Println(err)
	//}

	if guild.GameStateMsg.Exists() {

		//verify that the user is reacting to the state/status message
		if guild.GameStateMsg.IsReactionTo(m) {
			idMatched := false
			for color, e := range guild.StatusEmojis[true] {
				if e.ID == m.Emoji.ID {
					idMatched = true
					log.Printf("Player %s reacted with color %s", m.UserID, game.GetColorStringForInt(color))

					playerData := guild.AmongUsData.GetByColor(game.GetColorStringForInt(color))
					if playerData != nil {
						guild.UserData.UpdatePlayerData(m.UserID, playerData)
					}

					//then remove the player's reaction if we matched, or if we didn't
					err := s.MessageReactionRemove(m.ChannelID, m.MessageID, e.FormatForReaction(), m.UserID)
					if err != nil {
						log.Println(err)
					}
					break
				}
			}
			if !idMatched {
				//log.Println(m.Emoji.Name)
				if m.Emoji.Name == "❌" {
					log.Printf("Removing player %s", m.UserID)
					guild.UserData.ClearPlayerData(m.UserID)
					err := s.MessageReactionRemove(m.ChannelID, m.MessageID, "❌", m.UserID)
					if err != nil {
						log.Println(err)
					}
					idMatched = true
				}
			}
			//make sure to update any voice changes if they occurred
			if idMatched {
				guild.handleTrackedMembers(s, 0)
				guild.GameStateMsg.Edit(s, gameStateResponse(guild))
			}
		}
	}

}

// ToString returns a simple string representation of the current state of the guild
func (guild *GuildState) ToString() string {
	return fmt.Sprintf("%v", guild)
}

func (guild *GuildState) clearGameTracking(s *discordgo.Session) {
	//clear the discord user links to underlying player data
	guild.UserData.ClearAllPlayerData()

	//clears the base-level player data in memory
	guild.AmongUsData.ClearPlayerData()

	//reset all the tracking channels
	guild.Tracking.Reset()

	guild.GameStateMsg.Delete(s)

	guild.AmongUsData.SetPhase(game.LOBBY)
}
