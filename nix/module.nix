# NixOS module for the herald gateway. Mirrors the host conventions in
# marcustut/nixos: sops-managed EnvironmentFile, local PostgreSQL over the unix
# socket (peer auth), loopback binding, and an nginx/ACME front door.
#
# The HTTP API and the queue consumer are separate systemd services so the
# dispatcher can be scaled (workers) or restarted without touching the API.
{ config, lib, pkgs, ... }:

let
  cfg = config.services.herald;
in
{
  options.services.herald = {
    enable = lib.mkEnableOption "the herald agent event gateway";

    package = lib.mkOption {
      type = lib.types.package;
      description = "The herald package to run, from the herald flake.";
    };

    configFile = lib.mkOption {
      type = lib.types.path;
      description = "Path to gateway.toml (non-secret configuration).";
    };

    environmentFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = ''
        systemd EnvironmentFile, e.g. config.sops.secrets.herald-env.path,
        providing HERALD_DATABASE_URL, HERALD_SMTP_USERNAME and
        HERALD_SMTP_PASSWORD.
      '';
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "herald";
      description = "User the services run as.";
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "herald";
      description = "Group the services run as.";
    };

    database = {
      name = lib.mkOption {
        type = lib.types.str;
        default = "herald";
        description = ''
          Database name. Defaults to the service user so ensureDBOwnership
          applies; connect with peer auth over the socket, e.g.
          HERALD_DATABASE_URL=postgres:///herald?host=/run/postgresql
        '';
      };
      create = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Create the role and database via services.postgresql.";
      };
    };

    dispatcher = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Run the delivery-queue consumer as a separate service.";
      };
      workers = lib.mkOption {
        type = lib.types.ints.positive;
        default = 1;
        description = "Concurrent claim loops.";
      };
      interval = lib.mkOption {
        type = lib.types.str;
        default = "1s";
        description = "Poll interval.";
      };
    };

    domain = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = ''
        Public domain for the HTTP API. When set, an nginx virtual host with
        ACME is created and proxies to 127.0.0.1:<port>.
      '';
    };

    port = lib.mkOption {
      type = lib.types.port;
      default = 8787;
      description = "Loopback port; must match server.addr in gateway.toml.";
    };

    basePath = lib.mkOption {
      type = lib.types.str;
      default = "/api";
      description = ''
        URL path prefix the API is served under. "/" serves at the root;
        otherwise the prefix is stripped before proxying (so "/api" exposes
        https://<domain>/api/v1/events).
      '';
    };
  };

  config = lib.mkIf cfg.enable (lib.mkMerge [
    {
      users.users.${cfg.user} = {
        isSystemUser = true;
        group = cfg.group;
      };
      users.groups.${cfg.group} = { };

      environment.systemPackages = [ cfg.package ];

      systemd.services.herald = {
        description = "herald agent event gateway (HTTP API)";
        wantedBy = [ "multi-user.target" ];
        after = [ "network-online.target" "postgresql.service" ];
        wants = [ "network-online.target" ];
        serviceConfig = {
          ExecStart = "${cfg.package}/bin/herald serve --config ${cfg.configFile}";
          EnvironmentFile = lib.optional (cfg.environmentFile != null) cfg.environmentFile;
          User = cfg.user;
          Group = cfg.group;
          Restart = "on-failure";
          RestartSec = 2;
          NoNewPrivileges = true;
          ProtectSystem = "strict";
          ProtectHome = true;
          PrivateTmp = true;
          PrivateDevices = true;
          RestrictAddressFamilies = [ "AF_INET" "AF_INET6" "AF_UNIX" ];
        };
      };
    }

    (lib.mkIf cfg.dispatcher.enable {
      systemd.services.herald-dispatcher = {
        description = "herald delivery dispatcher (queue consumer)";
        wantedBy = [ "multi-user.target" ];
        after = [ "network-online.target" "postgresql.service" ];
        wants = [ "network-online.target" ];
        serviceConfig = {
          ExecStart = "${cfg.package}/bin/herald dispatch --config ${cfg.configFile} --workers ${toString cfg.dispatcher.workers} --interval ${cfg.dispatcher.interval}";
          EnvironmentFile = lib.optional (cfg.environmentFile != null) cfg.environmentFile;
          User = cfg.user;
          Group = cfg.group;
          Restart = "on-failure";
          RestartSec = 2;
          NoNewPrivileges = true;
          ProtectSystem = "strict";
          ProtectHome = true;
          PrivateTmp = true;
          PrivateDevices = true;
          RestrictAddressFamilies = [ "AF_INET" "AF_INET6" "AF_UNIX" ];
        };
      };
    })

    (lib.mkIf cfg.database.create {
      services.postgresql.ensureDatabases = [ cfg.database.name ];
      services.postgresql.ensureUsers = [{
        name = cfg.user;
        ensureDBOwnership = true;
      }];
    })

    (lib.mkIf (cfg.domain != null) {
      services.nginx.virtualHosts.${cfg.domain} = {
        forceSSL = true;
        enableACME = true;
        locations =
          if cfg.basePath == "/" || cfg.basePath == "" then {
            "/" = { proxyPass = "http://127.0.0.1:${toString cfg.port}"; };
          } else {
            "${cfg.basePath}/" = { proxyPass = "http://127.0.0.1:${toString cfg.port}/"; };
          };
      };
    })
  ]);
}
