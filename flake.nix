{
  description = "soundbooth — record, meter, and transcribe meetings in the terminal";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs =
    { self, nixpkgs }:
    let
      forSystems =
        f:
        nixpkgs.lib.genAttrs [ "aarch64-darwin" "x86_64-darwin" ] (
          system: f nixpkgs.legacyPackages.${system}
        );
    in
    {
      packages = forSystems (pkgs: rec {
        soundbooth = pkgs.rustPlatform.buildRustPackage {
          pname = "soundbooth";
          version = "0.4.0";
          src = ./.;
          cargoLock.lockFile = ./Cargo.lock;
          nativeBuildInputs = [ pkgs.makeWrapper ];
          env.SB_REV = self.shortRev or self.dirtyShortRev or "unknown";
          # sox encodes/plays and ffmpeg drives the armed-mode buffer; pin
          # both onto PATH so the binary works from a bare environment.
          # whispermlx cannot come from nix (nixpkgs MLX is CPU-only) —
          # install it with uv, see the README.
          postInstall = ''
            wrapProgram $out/bin/soundbooth \
              --prefix PATH : ${
                pkgs.lib.makeBinPath [
                  pkgs.sox
                  pkgs.ffmpeg
                ]
              }
          '';
          # tests exercise the microphone; not possible in the nix sandbox
          doCheck = false;
          meta = {
            description = "Terminal meeting recorder with replay buffer and on-device transcription";
            license = pkgs.lib.licenses.mit;
          };
        };
        default = soundbooth;
      });
    };
}
