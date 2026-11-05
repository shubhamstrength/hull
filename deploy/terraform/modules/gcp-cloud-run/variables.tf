variable "name" {
  description = "Cloud Run service name."
  type        = string
}

variable "project" {
  description = "Google Cloud project ID."
  type        = string
}

variable "region" {
  description = "Region to deploy to."
  type        = string
  default     = "us-central1"
}

variable "image" {
  description = "Image to run, ideally pinned by digest, e.g. us-central1-docker.pkg.dev/proj/repo/web@sha256:..."
  type        = string
}

variable "container_port" {
  description = "Port the container listens on."
  type        = number
  default     = 8080
}

variable "cpu" {
  description = "CPU limit, e.g. \"1\"."
  type        = string
  default     = "1"
}

variable "memory" {
  description = "Memory limit, e.g. \"512Mi\"."
  type        = string
  default     = "512Mi"
}

variable "min_instances" {
  description = "Instances kept warm (0 scales to zero)."
  type        = number
  default     = 0
}

variable "max_instances" {
  description = "Upper bound on instances."
  type        = number
  default     = 3
}

variable "environment" {
  description = "Environment variables for the container."
  type        = map(string)
  default     = {}
}

variable "public" {
  description = "Allow unauthenticated requests."
  type        = bool
  default     = true
}
