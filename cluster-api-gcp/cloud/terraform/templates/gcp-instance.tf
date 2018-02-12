variable "project" {
  type = "string"
}

variable "instance_name" {
  type = "string"
}

variable "instance_type" {
  type = "string"
}

variable "instance_zone" {
  type = "string"
}

variable "uid" {
  type = "string"
}

variable "startup_script" {
  type = "string"
}

provider "google" {
  project     = "${var.project}"
  region      = "us-central1"
}

// Create a new instance
resource "google_compute_instance" "default" {
  name         = "${var.instance_name}"
  machine_type = "${var.instance_type}"
  zone         = "${var.instance_zone}"

  tags = [ "https-server" ]

  boot_disk {
    initialize_params {
      image = "projects/ubuntu-os-cloud/global/images/family/ubuntu-1604-lts"
    }
  }

  // Local SSD disk
  scratch_disk {
  }

  network_interface {
    network = "default"

    access_config {
      // Ephemeral IP
    }
  }

  metadata {
    foo = "bar"
  }

  labels {
    "machine-crd-uid" = "${var.uid}"
  }

  metadata_startup_script = "${var.startup_script}"

  service_account {
    scopes = ["userinfo-email", "compute-ro", "storage-ro"]
  }
}