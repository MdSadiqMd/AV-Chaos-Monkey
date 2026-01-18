package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
)

func findParticipantsInPod(httpClient *http.Client, podURL, testID string, partitionID int, maxCount int) []uint32 {
	metricsURL := fmt.Sprintf("%s/api/v1/test/%s/metrics", podURL, testID)
	resp, err := httpClient.Get(metricsURL)
	if err == nil && resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		var metrics map[string]any
		if json.NewDecoder(resp.Body).Decode(&metrics) == nil {
			if participants, ok := metrics["participants"].([]any); ok {
				participantIDs := make([]uint32, 0, len(participants))
				for _, p := range participants {
					if pMap, ok := p.(map[string]any); ok {
						if pid, ok := pMap["participant_id"].(float64); ok {
							participantIDs = append(participantIDs, uint32(pid))
							if maxCount > 0 && len(participantIDs) >= maxCount {
								break
							}
						}
					}
				}
				if len(participantIDs) > 0 {
					return participantIDs
				}
			}
		}
	}
	return probeParticipants(httpClient, podURL, testID, partitionID, maxCount)
}

func findParticipants(httpClient *http.Client, baseURL, testID string, maxCount int) []uint32 {
	metricsURL := fmt.Sprintf("%s/api/v1/test/%s/metrics", baseURL, testID)
	resp, err := httpClient.Get(metricsURL)
	if err == nil && resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		var metrics map[string]any
		if json.NewDecoder(resp.Body).Decode(&metrics) == nil {
			if participants, ok := metrics["participants"].([]any); ok {
				participantIDs := make([]uint32, 0, len(participants))
				for _, p := range participants {
					if pMap, ok := p.(map[string]any); ok {
						if pid, ok := pMap["participant_id"].(float64); ok {
							participantIDs = append(participantIDs, uint32(pid))
							if maxCount > 0 && len(participantIDs) >= maxCount {
								break
							}
						}
					}
				}
				if len(participantIDs) > 0 {
					return participantIDs
				}
			}
		}
	}
	return probeParticipantsSingle(httpClient, baseURL, testID, maxCount)
}

func probeParticipants(httpClient *http.Client, podURL, testID string, partitionID int, maxCount int) []uint32 {
	totalPartitions := getTotalPartitions(httpClient, podURL)
	participantIDs := make([]uint32, 0)
	baseID := uint32(1001)
	searchLimit := 10000
	if maxCount > 0 {
		searchLimit = maxCount * totalPartitions * 2
	}

	logging.LogInfo("Searching for participants in partition %d", partitionID)
	checked, found := 0, 0

	for i := 0; i < searchLimit; i++ {
		candidateID := baseID + uint32(i)
		if int(candidateID)%totalPartitions != partitionID {
			continue
		}
		checked++

		url := fmt.Sprintf("%s/api/v1/test/%s/sdp/%d", podURL, testID, candidateID)
		resp, err := httpClient.Get(url)
		if err != nil {
			continue
		}
		if resp.StatusCode == http.StatusOK {
			participantIDs = append(participantIDs, candidateID)
			found++
			if maxCount > 0 && len(participantIDs) >= maxCount {
				resp.Body.Close()
				break
			}
		} else if resp.StatusCode != http.StatusNotFound {
			body, _ := io.ReadAll(resp.Body)
			logging.LogWarning("Unexpected status %d for participant %d: %s", resp.StatusCode, candidateID, string(body))
		}
		resp.Body.Close()
		if found == 0 && checked > 100 {
			break
		}
	}
	logging.LogInfo("Found %d participants for partition %d", len(participantIDs), partitionID)
	return participantIDs
}

func probeParticipantsSingle(httpClient *http.Client, baseURL, testID string, maxCount int) []uint32 {
	searchRange := maxCount * 20
	if maxCount < 0 {
		searchRange = 10000
	}
	participantIDs := make([]uint32, 0)
	for i := 0; i < searchRange; i++ {
		if maxCount > 0 && len(participantIDs) >= maxCount {
			break
		}
		pid := uint32(1001 + i)
		url := fmt.Sprintf("%s/api/v1/test/%s/sdp/%d", baseURL, testID, pid)
		resp, err := httpClient.Get(url)
		if err != nil {
			continue
		}
		if resp.StatusCode == http.StatusOK {
			participantIDs = append(participantIDs, pid)
		}
		resp.Body.Close()
	}
	return participantIDs
}

func getTotalPartitions(httpClient *http.Client, podURL string) int {
	totalPartitions := 10
	healthURL := fmt.Sprintf("%s/healthz", podURL)
	resp, err := httpClient.Get(healthURL)
	if err == nil && resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		var health map[string]any
		if json.NewDecoder(resp.Body).Decode(&health) == nil {
			if tp, ok := health["total_partitions"].(float64); ok {
				totalPartitions = int(tp)
			}
		}
	}
	return totalPartitions
}
