package main

import (
	"database/sql"
	"encoding/json"
	"io/ioutil"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/ovh/cds/engine/api/context"
	"github.com/ovh/cds/engine/api/pipeline"
	"github.com/ovh/cds/engine/log"
	"github.com/ovh/cds/sdk"
)

func getParametersInPipelineHandler(w http.ResponseWriter, r *http.Request, db *sql.DB, c *context.Context) {

	vars := mux.Vars(r)
	key := vars["key"]
	pipelineName := vars["permPipelineKey"]

	p, err := pipeline.LoadPipeline(db, key, pipelineName, false)
	if err != nil {
		log.Warning("getParametersInPipelineHandler: Cannot load %s: %s\n", pipelineName, err)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	parameters, err := pipeline.GetAllParametersInPipeline(db, p.ID)
	if err != nil {
		log.Warning("getParametersInPipelineHandler: Cannot get parameters for pipeline %s: %s\n", pipelineName, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	WriteJSON(w, r, parameters, http.StatusOK)

}

func deleteParameterFromPipelineHandler(w http.ResponseWriter, r *http.Request, db *sql.DB, c *context.Context) {

	vars := mux.Vars(r)
	key := vars["key"]
	pipelineName := vars["permPipelineKey"]
	paramName := vars["name"]

	p, err := pipeline.LoadPipeline(db, key, pipelineName, false)
	if err != nil {
		log.Warning("deleteParameterFromPipelineHandler: Cannot load %s: %s\n", pipelineName, err)
		WriteError(w, r, err)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Warning("deleteParameterFromPipelineHandler: Cannot start transaction: %s\n", err)
		WriteError(w, r, err)
		return
	}
	defer tx.Rollback()

	err = pipeline.DeleteParameterFromPipeline(tx, p.ID, paramName)
	if err != nil {
		log.Warning("deleteParameterFromPipelineHandler: Cannot delete %s: %s\n", paramName, err)
		WriteError(w, r, err)
		return
	}

	err = pipeline.UpdatePipelineLastModified(tx, p.ID)
	if err != nil {
		log.Warning("deleteParameterFromPipelineHandler> Cannot update pipeline last_modified date: %s", err)
		WriteError(w, r, err)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Warning("deleteParameterFromPipelineHandler: Cannot commit transaction: %s\n", err)
		WriteError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func updateParametersInPipelineHandler(w http.ResponseWriter, r *http.Request, db *sql.DB, c *context.Context) {
	vars := mux.Vars(r)
	key := vars["key"]
	pipelineName := vars["permPipelineKey"]

	// Get body
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var pipParams []sdk.Parameter
	err = json.Unmarshal(data, &pipParams)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	pip, err := pipeline.LoadPipeline(db, key, pipelineName, false)
	if err != nil {
		log.Warning("updateParametersInPipelineHandler: Cannot load %s: %s\n", pipelineName, err)
		WriteError(w, r, err)
		return
	}
	pip.Parameter, err = pipeline.GetAllParametersInPipeline(db, pip.ID)
	if err != nil {
		log.Warning("updateParametersInPipelineHandler> Cannot GetAllParametersInPipeline: %s\n", err)
		WriteError(w, r, err)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Warning("updateParametersInPipelineHandler: Cannot start transaction: %s", err)
		WriteError(w, r, sdk.ErrUnknownError)
		return
	}
	defer tx.Rollback()

	// Check with exising parameter to know whether parameter has been deleted, update or added
	var deleted, updated, added []sdk.Parameter
	var found bool
	for _, p := range pip.Parameter {
		found = false
		for _, new := range pipParams {
			// If we found a parameter with the same id but different value, then its modified
			if p.ID == new.ID {
				updated = append(updated, new)
				found = true
				break
			}
		}
		// If parameter is not found in new batch, then it  has been deleted
		if !found {
			deleted = append(deleted, p)
		}
	}

	// Added parameter are the one present in new batch but not in db
	for _, new := range pipParams {
		found = false
		for _, p := range pip.Parameter {
			if p.ID == new.ID {
				found = true
				break
			}
		}
		if !found {
			added = append(added, new)
		}
	}

	// Ok now permform actual update
	for i := range added {
		p := &added[i]
		err = pipeline.InsertParameterInPipeline(tx, pip.ID, p)
		if err != nil {
			log.Warning("UpdatePipelineParameters> Cannot insert new params %s: %s", p.Name, err)
			WriteError(w, r, err)
			return
		}
	}
	for _, p := range updated {
		err = pipeline.UpdateParameterInPipeline(tx, pip.ID, p)
		if err != nil {
			log.Warning("UpdatePipelineParameters> Cannot update parameter %s: %s", p.Name, err)
			WriteError(w, r, err)
			return
		}
	}
	for _, p := range deleted {
		err = pipeline.DeleteParameterFromPipeline(tx, pip.ID, p.Name)
		if err != nil {
			log.Warning("UpdatePipelineParameters> Cannot delete parameter %s: %s", p.Name, err)
			WriteError(w, r, err)
			return
		}
	}

	query := `
			UPDATE application
			SET last_modified = current_timestamp
			FROM application_pipeline
			WHERE application_pipeline.application_id = application.id
			AND application_pipeline.pipeline_id = $1
		`
	_, err = tx.Exec(query, pip.ID)
	if err != nil {
		log.Warning("UpdatePipelineParameters> Cannot update linked application [%d]: %s", pip.ID, err)
		WriteError(w, r, err)
		return
	}

	err = pipeline.UpdatePipelineLastModified(tx, pip.ID)
	if err != nil {
		log.Warning("UpdatePipelineParameters> Cannot update pipeline last_modified date: %s", err)
		WriteError(w, r, err)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Warning("updateParametersInPipelineHandler: Cannot commit transaction: %s", err)
		WriteError(w, r, sdk.ErrUnknownError)
		return
	}

	WriteJSON(w, r, append(added, updated...), http.StatusOK)
}

func updateParameterInPipelineHandler(w http.ResponseWriter, r *http.Request, db *sql.DB, c *context.Context) {
	vars := mux.Vars(r)
	key := vars["key"]
	pipelineName := vars["permPipelineKey"]
	paramName := vars["name"]

	// Get body
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	var newParam sdk.Parameter
	err = json.Unmarshal(data, &newParam)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if newParam.Name != paramName {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	p, err := pipeline.LoadPipeline(db, key, pipelineName, false)
	if err != nil {
		log.Warning("updateParameterInPipelineHandler: Cannot load %s: %s\n", pipelineName, err)
		WriteError(w, r, err)
		return
	}

	paramInPipeline, err := pipeline.CheckParameterInPipeline(db, p.ID, paramName)
	if err != nil {
		log.Warning("updateParameterInPipelineHandler: Cannot check if parameter %s is already in the pipeline %s: %s\n", paramName, pipelineName, err)
		WriteError(w, r, err)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Warning("updateParameterInPipelineHandler: Cannot start transaction:  %s\n", err)
		WriteError(w, r, err)
		return
	}
	defer tx.Rollback()

	if paramInPipeline {
		err := pipeline.UpdateParameterInPipeline(tx, p.ID, newParam)
		if err != nil {
			log.Warning("updateParameterInPipelineHandler: Cannot update parameter %s in pipeline %s:  %s\n", paramName, pipelineName, err)
			WriteError(w, r, err)
			return
		}
	}

	err = pipeline.UpdatePipelineLastModified(tx, p.ID)
	if err != nil {
		log.Warning("updateParameterInPipelineHandler: Cannot update pipeline last_modified date:  %s\n", err)
		WriteError(w, r, err)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Warning("updateParameterInPipelineHandler: Cannot commit transaction:  %s\n", err)
		WriteError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func addParameterInPipelineHandler(w http.ResponseWriter, r *http.Request, db *sql.DB, c *context.Context) {

	vars := mux.Vars(r)
	key := vars["key"]
	pipelineName := vars["permPipelineKey"]
	paramName := vars["name"]

	// Get body
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Warning("addParameterInPipelineHandler> Cannot read body: %s", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var newParam sdk.Parameter
	err = json.Unmarshal(data, &newParam)
	if err != nil {
		log.Warning("addParameterInPipelineHandler> Cannot unmarshal body: %s", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if newParam.Name != paramName {
		log.Warning("addParameterInPipelineHandler> Wrong param name got %s instead of %s", newParam.Name, paramName)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	p, err := pipeline.LoadPipeline(db, key, pipelineName, false)
	if err != nil {
		log.Warning("addParameterInPipelineHandler: Cannot load %s: %s\n", pipelineName, err)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	paramInProject, err := pipeline.CheckParameterInPipeline(db, p.ID, paramName)
	if err != nil {
		log.Warning("addParameterInPipelineHandler: Cannot check if parameter %s is already in the pipeline %s: %s\n", paramName, pipelineName, err)
		WriteError(w, r, err)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Warning("addParameterInPipelineHandler: Cannot start transaction: %s\n", err)
		WriteError(w, r, err)
		return
	}
	defer tx.Rollback()

	if !paramInProject {
		err := pipeline.InsertParameterInPipeline(tx, p.ID, &newParam)
		if err != nil {
			log.Warning("addParameterInPipelineHandler: Cannot add parameter %s in pipeline %s:  %s\n", paramName, pipelineName, err)
			WriteError(w, r, err)
			return
		}
	}

	err = pipeline.UpdatePipelineLastModified(tx, p.ID)
	if err != nil {
		log.Warning("addParameterInPipelineHandler> Cannot update pipeline last_modified date: %s", err)
		WriteError(w, r, err)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Warning("addParameterInPipelineHandler: Cannot commit transaction: %s\n", err)
		WriteError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}
