# Integrating a game

Use the independent [Unity companion package](https://github.com/xuhuanhello/agones-server-nakama-plugin-unity). Implement its business host contract, then use its minimal-server and Nakama-client samples. Its lifecycle bridge handles SDK coordination, but cannot infer your room teardown, authoritative input handling, durable result persistence or reconnect reservations.

Keep all Agones credentials in the dedicated server build. Clients call the normal authenticated Nakama SDK RPC API and attach the returned short ticket to their game transport admission. No official Nakama SDK or source modifications are required. A game still needs explicit adapter code: installing the package alone does not turn an arbitrary scene into a managed server.

For DM, integrate behind a separate provider profile. Bind FishNet UDP before readiness, apply SDK port configuration, route room/seat ticket validation into its authenticator, emit business-loop health pulses and real metrics, and keep manual network compatibility independent of executable build digest. Validate match/cancel/leave/reconnect/rematch, both editor platforms and Android, against the same profile before promoting it. This repository contains no private DM game source.
