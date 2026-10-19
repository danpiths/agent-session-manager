{
  description = "Agent Session Manager development environment";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux" ];
    in {
      devShells = nixpkgs.lib.genAttrs systems (system:
        let
          pkgs = import nixpkgs {
            inherit system;
            config.allowUnfreePredicate = pkg: builtins.elem (nixpkgs.lib.getName pkg) [ "claude-code" ];
          };
          # Official release binaries, including static musl builds on Linux.
          forgeAsset = {
            aarch64-darwin = { target = "aarch64-apple-darwin"; sha256 = "cf1137552122c3477a9763e3a76d29464ea3863645bcaf1876925356a2266f47"; };
            x86_64-darwin = { target = "x86_64-apple-darwin"; sha256 = "b42b2abc6936b113e8c60b8c44e3c0e5673fd97c3d7cceebd1fb8b83097149fe"; };
            aarch64-linux = { target = "aarch64-unknown-linux-musl"; sha256 = "f1a89a2fea3f88f08eedef72aaf7be4929f47fd3f10bf86eca720aa15433e7da"; };
            x86_64-linux = { target = "x86_64-unknown-linux-musl"; sha256 = "64832f828bf08961322e302243f352ee7c27e88d76f972574a3628c7b70cb1be"; };
          }.${system};
          forgecode = pkgs.stdenvNoCC.mkDerivation rec {
            pname = "forgecode";
            version = "2.14.0";
            src = pkgs.fetchurl {
              url = "https://github.com/tailcallhq/forgecode/releases/download/v${version}/forge-${forgeAsset.target}";
              inherit (forgeAsset) sha256;
            };
            dontUnpack = true;
            installPhase = ''
              runHook preInstall
              install -Dm755 "$src" "$out/bin/forge"
              runHook postInstall
            '';
          };
        in {
          default = pkgs.mkShell {
            packages = [ pkgs.go_1_26 pkgs.gopls pkgs.gotools pkgs.git pkgs.python3 pkgs.zip pkgs.unzip pkgs.codex pkgs.claude-code forgecode ];
            GOTOOLCHAIN = "auto";
          };
        });
    };
}
