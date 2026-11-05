output "url" {
  description = "Public URL of the service."
  value       = google_cloud_run_v2_service.this.uri
}
