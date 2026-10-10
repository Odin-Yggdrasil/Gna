//! Outil de test : `relay <adresse>` affiche l'empreinte ; `relay <adresse> <empreinte>` démarre le relais.
use gna_relay::{probe, start, Upstream};

#[tokio::main]
async fn main() {
    let args: Vec<String> = std::env::args().collect();
    let up = Upstream::parse(&args[1]).expect("adresse");
    if args.len() < 3 {
        match probe(&up).await {
            Ok(fp) => println!("{fp}"),
            Err(e) => eprintln!("erreur : {e}"),
        }
        return;
    }
    let h = start(up, &args[2], 0, Some(std::sync::Arc::new(|| eprintln!("réglages demandés")))).await.unwrap();
    println!("{}", h.url());
    std::future::pending::<()>().await;
}
