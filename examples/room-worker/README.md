# Local protocol fixture

This small Python process exercises real Agones SDK lifecycle calls, Nakama room commands and signed tickets over UDP. It is used only in the isolated integration harness. It does not implement FishNet, Unity physics, production reconnection policy, persistent match results or game performance measurement. Do not publish it as your game image or expose it to real players.
