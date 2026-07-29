# Home-manager module for the hoard desktop agent. Exposed as
# `homeManagerModules.hoard-agent` from the flake; `self` is the flake so we can
# reach the built package for the current system.
self:
{ config, lib, pkgs, ... }:
let
  cfg = config.services.hoard-agent;
in {
  options.services.hoard-agent = {
    enable = lib.mkEnableOption "hoard desktop backup agent";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.hoard-agent;
      description = "The hoard-agent package to run.";
    };

    repository = lib.mkOption {
      type = lib.types.str;
      example = "rest:http://truenas:8000/hot";
      description = "hoard server restic REST URL the agent pushes to.";
    };

    passwordFile = lib.mkOption {
      type = lib.types.str;
      example = "/run/secrets/hoard_hot_password";
      description = ''
        Path to a file containing the hot-repo password. Provide this via a
        secret manager (sops-nix, agenix); it is read at runtime and never
        placed in the Nix store.
      '';
    };

    host = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "Override the snapshot hostname (defaults to the machine hostname).";
    };

    listen = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:7420";
      description = "Address the local web GUI binds to.";
    };

    restic = lib.mkOption {
      type = lib.types.package;
      default = pkgs.restic;
      description = "restic package the agent shells out to.";
    };
  };

  config = lib.mkIf cfg.enable {
    home.packages = [ cfg.package ];

    systemd.user.services.hoard-agent = {
      Unit = {
        Description = "hoard desktop backup agent (GUI + scheduler)";
        After = [ "network-online.target" ];
        Wants = [ "network-online.target" ];
      };
      Service = {
        # repository / password file / host SEED the agent's settings on first
        # run; the Settings panel in the GUI is the source of truth and can
        # change them thereafter (all settings persist to the config JSON).
        Environment = [
          "HOARD_AGENT_REPOSITORY=${cfg.repository}"
          "HOARD_AGENT_PASSWORD_FILE=${cfg.passwordFile}"
        ] ++ lib.optional (cfg.host != "") "HOARD_AGENT_HOST=${cfg.host}";
        ExecStart = "${lib.getExe cfg.package} -listen ${cfg.listen} -restic ${lib.getExe cfg.restic}";
        # "always" (not "on-failure") so the agent comes back even after a clean
        # kill; an explicit `systemctl --user stop` still stops it for good.
        Restart = "always";
        RestartSec = 5;
      };
      Install.WantedBy = [ "default.target" ];
    };
  };
}
