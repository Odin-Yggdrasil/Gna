// Pas de console supplémentaire sous Windows en version finale.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

fn main() {
    gna_app_lib::run()
}
