# Deploys an image built and pushed with hull to Google Cloud Run.
#
#   1. terraform init && terraform apply -var project=my-project -target=google_artifact_registry_repository.app
#   2. gcloud auth print-access-token | hull login -u oauth2accesstoken --password-stdin us-central1-docker.pkg.dev
#      hull push --digest-file image.txt      (with image: set to the repository output plus /<name> in hull.yaml)
#   3. terraform apply -var project=my-project -var "image=$(cat image.txt)"

terraform {
  required_version = ">= 1.5"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 5.0, < 7.0"
    }
  }
}

variable "project" {
  type = string
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "name" {
  type    = string
  default = "react-app"
}

variable "image" {
  description = "Image reference printed by `hull push`, e.g. <repo>/react-app@sha256:..."
  type        = string
  default     = ""
}

provider "google" {
  project = var.project
  region  = var.region
}

resource "google_artifact_registry_repository" "app" {
  repository_id = var.name
  location      = var.region
  format        = "DOCKER"
}

module "app" {
  source = "../../modules/gcp-cloud-run"
  count  = var.image == "" ? 0 : 1

  name    = var.name
  project = var.project
  region  = var.region
  image   = var.image
}

output "repository" {
  description = "Set image: in hull.yaml to this plus /<name>."
  value       = "${var.region}-docker.pkg.dev/${var.project}/${google_artifact_registry_repository.app.repository_id}"
}

output "url" {
  value = try(module.app[0].url, "push an image and apply again with -var image=...")
}
