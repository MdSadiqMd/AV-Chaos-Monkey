{
  description = "AV Chaos Monkey - WebRTC/RTP chaos testing orchestrator";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    let
      defaultOutputs = flake-utils.lib.eachDefaultSystem (
        system:
        let
          pkgs = import nixpkgs {
            inherit system;
          };

          buildApp =
            {
              goPackage ? pkgs.go_1_24,
            }:
            pkgs.buildGoModule {
              pname = "av-chaos-monkey";
              version = "1.0.0";
              src = ./.;

              # Specify which Go to use
              go = goPackage;

              # Go module dependencies
              vendorHash = "sha256-HFUnpGpJUy7Y5ofMGcEio5FZVBbEfwxFFbVWxzoDlSk=";

              # Build flags
              ldflags = [
                "-s"
                "-w"
                "-X main.version=${self.rev or "dev"}"
              ];

              # Build the main binary
              subPackages = [ "cmd/main.go" ];

              # CGO disabled for static binary (set via preBuild)
              preBuild = ''
                export CGO_ENABLED=0
              '';

              # Meta information
              meta = with pkgs.lib; {
                description = "WebRTC/RTP chaos testing orchestrator";
                homepage = "https://github.com/MdSadiqMd/AV-Chaos-Monkey";
                license = licenses.mit;
                maintainers = [ ];
                platforms = platforms.unix;
              };
            };

          # Build the Go application for current system
          av-chaos-monkey = buildApp { };

          # Development shell with all tools
          devShell = pkgs.mkShell {
            buildInputs = with pkgs; [
              # Go toolchain
              go_1_24
              gopls
              gotools
              go-tools

              # Build tools
              gnumake
              git
              curl
              jq

              # Container tools
              docker
              docker-compose
              kubectl
              kubernetes-helm
              kind # Kubernetes in Docker - runs K8s cluster inside Docker

              # Protobuf tools
              protobuf
              protoc-gen-go
              protoc-gen-go-grpc

              # Script dependencies
              bash
              coreutils
              gnused
              gawk
              bc

              # Optional: Nix development tools
              nixpkgs-fmt
              statix
            ];

            shellHook = ''
              echo "🚀 AV Chaos Monkey Development Environment"
              echo ""
              echo "Available commands:"
              echo "  nix develop          - Enter this development shell"
              echo "  nix build            - Build the application"
              echo ""
              echo "Cross-compilation (from any system):"
              echo "  nix build .#packages.x86_64-linux.av-chaos-monkey      - Linux x86_64"
              echo "  nix build .#packages.aarch64-linux.av-chaos-monkey       - Linux ARM64"
              echo "  nix build .#packages.x86_64-darwin.av-chaos-monkey      - macOS Intel"
              echo "  nix build .#packages.aarch64-darwin.av-chaos-monkey     - macOS Apple Silicon"
              echo ""
              echo "Go version: $(go version)"
              echo "Docker: $(docker --version 2>/dev/null || echo 'not available')"
              echo "Kubectl: $(kubectl version --client --short 2>/dev/null || echo 'not available')"
              echo "Kind: $(kind --version 2>/dev/null || echo 'not available')"
              echo ""
              echo "Kubernetes cluster management:"
              echo "  kind create cluster --name av-chaos-monkey  - Create K8s cluster"
              echo "  kind delete cluster --name av-chaos-monkey   - Delete K8s cluster"
              echo "  kubectl cluster-info --context kind-av-chaos-monkey  - Check cluster"
              echo ""
            '';
          };

          # Package with scripts wrapper
          av-chaos-monkey-with-scripts = pkgs.stdenv.mkDerivation {
            pname = "av-chaos-monkey-full";
            version = "1.0.0";
            src = ./.;

            buildInputs = [ av-chaos-monkey ];

            installPhase = ''
              mkdir -p $out/bin
              mkdir -p $out/share/av-chaos-monkey

              # Install the binary
              cp ${av-chaos-monkey}/bin/main $out/bin/av-chaos-monkey
              chmod +x $out/bin/av-chaos-monkey

              # Install scripts
              cp -r scripts $out/share/av-chaos-monkey/
              chmod +x $out/share/av-chaos-monkey/scripts/*.sh

              # Create wrapper scripts in bin
              cat > $out/bin/start-k8s <<EOF
              #!${pkgs.bash}/bin/bash
              exec $out/share/av-chaos-monkey/scripts/start_everything.sh k8s "\$@"
              EOF

              cat > $out/bin/cleanup <<EOF
              #!${pkgs.bash}/bin/bash
              exec $out/share/av-chaos-monkey/scripts/cleanup.sh "\$@"
              EOF

              cat > $out/bin/start-everything <<EOF
              #!${pkgs.bash}/bin/bash
              exec $out/share/av-chaos-monkey/scripts/start_everything.sh "\$@"
              EOF

              chmod +x $out/bin/start-k8s
              chmod +x $out/bin/cleanup
              chmod +x $out/bin/start-everything

              # Install config files
              cp -r config $out/share/av-chaos-monkey/
              cp -r k8s $out/share/av-chaos-monkey/
              cp -r proto $out/share/av-chaos-monkey/
              cp docker-compose.yaml $out/share/av-chaos-monkey/
              cp Dockerfile $out/share/av-chaos-monkey/
            '';

            meta = with pkgs.lib; {
              description = "AV Chaos Monkey with scripts and configs";
              homepage = "https://github.com/MdSadiqMd/AV-Chaos-Monkey";
              license = licenses.mit;
              platforms = platforms.unix;
            };
          };

        in
        {
          # Default package
          packages.default = av-chaos-monkey;

          # Individual packages
          packages.av-chaos-monkey = av-chaos-monkey;
          packages.full = av-chaos-monkey-with-scripts;

          # Development shell
          devShells.default = devShell;

          # Apps (runnable commands)
          apps.default = {
            type = "app";
            program = "${av-chaos-monkey}/bin/main";
          };
        }
      );

      # Cross-compilation packages - build for specific systems from any host
      crossPackages = {
        x86_64-linux =
          let
            pkgs = import nixpkgs { system = "x86_64-linux"; };
          in
          {
            av-chaos-monkey = pkgs.buildGoModule {
              pname = "av-chaos-monkey";
              version = "1.0.0";
              src = ./.;
              vendorHash = "sha256-HFUnpGpJUy7Y5ofMGcEio5FZVBbEfwxFFbVWxzoDlSk=";
              preBuild = "export CGO_ENABLED=0";
              subPackages = [ "cmd/main.go" ];
              ldflags = [
                "-s"
                "-w"
              ];
            };
          };

        aarch64-linux =
          let
            pkgs = import nixpkgs { system = "aarch64-linux"; };
          in
          {
            av-chaos-monkey = pkgs.buildGoModule {
              pname = "av-chaos-monkey";
              version = "1.0.0";
              src = ./.;
              vendorHash = "sha256-HFUnpGpJUy7Y5ofMGcEio5FZVBbEfwxFFbVWxzoDlSk=";
              preBuild = "export CGO_ENABLED=0";
              subPackages = [ "cmd/main.go" ];
              ldflags = [
                "-s"
                "-w"
              ];
            };
          };

        x86_64-darwin =
          let
            pkgs = import nixpkgs { system = "x86_64-darwin"; };
          in
          {
            av-chaos-monkey = pkgs.buildGoModule {
              pname = "av-chaos-monkey";
              version = "1.0.0";
              src = ./.;
              vendorHash = "sha256-HFUnpGpJUy7Y5ofMGcEio5FZVBbEfwxFFbVWxzoDlSk=";
              preBuild = "export CGO_ENABLED=0";
              subPackages = [ "cmd/main.go" ];
              ldflags = [
                "-s"
                "-w"
              ];
            };
          };

        aarch64-darwin =
          let
            pkgs = import nixpkgs { system = "aarch64-darwin"; };
          in
          {
            av-chaos-monkey = pkgs.buildGoModule {
              pname = "av-chaos-monkey";
              version = "1.0.0";
              src = ./.;
              vendorHash = "sha256-HFUnpGpJUy7Y5ofMGcEio5FZVBbEfwxFFbVWxzoDlSk=";
              preBuild = "export CGO_ENABLED=0";
              subPackages = [ "cmd/main.go" ];
              ldflags = [
                "-s"
                "-w"
              ];
            };
          };
      };
    in
    # Merge default outputs with cross-compilation packages
    let
      pkgsForLib = import nixpkgs { system = "x86_64-linux"; };
    in
    pkgsForLib.lib.recursiveUpdate defaultOutputs {
      packages = crossPackages;
    };
}
