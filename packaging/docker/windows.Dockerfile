# Environnement de compilation de l'installeur Windows de l'application Gná, depuis Linux :
# image Tauri d'Andvari + MinGW, NSIS et Rust avec la cible x86_64-pc-windows-gnu.
# Tout reste dans l'image : rien n'est installé sur la machine hôte.
FROM andvari-tauri-build
RUN apt-get update && apt-get install -y --no-install-recommends mingw-w64 nsis \
    && rm -rf /var/lib/apt/lists/*
ENV RUSTUP_HOME=/opt/rustup CARGO_HOME=/opt/cargo PATH=/opt/cargo/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
      | sh -s -- -y --profile minimal --default-toolchain stable --target x86_64-pc-windows-gnu --no-modify-path \
    && chmod -R a+rwX /opt/cargo /opt/rustup
