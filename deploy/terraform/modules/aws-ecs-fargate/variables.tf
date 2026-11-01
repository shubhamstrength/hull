variable "name" {
  description = "Name for the service and the resources created for it."
  type        = string
}

variable "image" {
  description = "Image to run, ideally pinned by digest, e.g. 123456789012.dkr.ecr.us-east-1.amazonaws.com/web@sha256:... (hull push prints it)."
  type        = string
}

variable "container_port" {
  description = "Port the container listens on."
  type        = number
  default     = 8080
}

variable "cpu" {
  description = "Fargate CPU units (256 = 0.25 vCPU)."
  type        = number
  default     = 256
}

variable "memory" {
  description = "Fargate memory in MiB."
  type        = number
  default     = 512
}

variable "cpu_architecture" {
  description = "X86_64 for linux/amd64 images, ARM64 for linux/arm64 images."
  type        = string
  default     = "X86_64"
}

variable "desired_count" {
  description = "Number of running copies."
  type        = number
  default     = 1
}

variable "environment" {
  description = "Environment variables for the container."
  type        = map(string)
  default     = {}
}

variable "vpc_id" {
  description = "VPC to run in."
  type        = string
}

variable "subnet_ids" {
  description = "Subnets for the load balancer and tasks. Use public subnets unless you have a NAT gateway."
  type        = list(string)
}

variable "assign_public_ip" {
  description = "Give tasks a public IP so they can pull the image without a NAT gateway."
  type        = bool
  default     = true
}

variable "health_check_path" {
  description = "Path the load balancer checks."
  type        = string
  default     = "/"
}

variable "log_retention_days" {
  description = "How long to keep container logs."
  type        = number
  default     = 14
}

variable "tags" {
  description = "Tags added to every resource."
  type        = map(string)
  default     = {}
}
